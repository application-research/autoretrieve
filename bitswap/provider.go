package bitswap

import (
	"context"
	"errors"
	"time"

	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/metrics"
	lassieretriever "github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/ipfs/go-bitswap/message"
	bitswap_message_pb "github.com/ipfs/go-bitswap/message/pb"
	"github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-peertaskqueue"
	"github.com/ipfs/go-peertaskqueue/peertask"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
)

var log = logging.Logger("provider")

// Wantlist want type redeclarations
const (
	wantTypeHave  = bitswap_message_pb.Message_Wantlist_Have
	wantTypeBlock = bitswap_message_pb.Message_Wantlist_Block
)

// Response queue actions
type ResponseAction uint

type ResponseData struct {
	action ResponseAction
	reason string
}

const (
	sendHave ResponseAction = iota
	sendDontHave
	sendBlock
)

const targetMessageSize = 1 << 10
const sendBlockThreshold = 2 << 10

type ProviderConfig struct {
	CidBlacklist     map[cid.Cid]bool
	RequestWorkers   uint
	ResponseWorkers  uint
	RetrievalWorkers uint
	RoutingTableType RoutingTableType
}

type Provider struct {
	config       ProviderConfig
	network      network.BitSwapNetwork
	blockManager *blocks.Manager
	retriever    *lassieretriever.Retriever

	// Incoming messages to be processed - work is 1 per message
	requestQueue *peertaskqueue.PeerTaskQueue

	// Outgoing messages to be sent - work is size of messages in bytes
	responseQueue *peertaskqueue.PeerTaskQueue

	// CIDs that need to be retrieved - work is 1 per CID queued
	retrievalQueue *peertaskqueue.PeerTaskQueue
}

type overwriteTaskMerger struct{}

func (*overwriteTaskMerger) HasNewInfo(task peertask.Task, existing []*peertask.Task) bool {
	return true
}

func (*overwriteTaskMerger) Merge(task peertask.Task, existing *peertask.Task) {
	*existing = task
}

func NewProvider(
	ctx context.Context,
	config ProviderConfig,
	host host.Host,
	datastore datastore.Batching,
	blockManager *blocks.Manager,
	retriever *lassieretriever.Retriever,
) (*Provider, error) {

	var routing routing.ContentRouting

	rtCfg := []dht.Option{
		dht.Datastore(datastore),
		dht.BucketSize(20),
		dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...),
	}

	switch config.RoutingTableType {
	case RoutingTableTypeDisabled:
	case RoutingTableTypeFull:
		fullRT, err := fullrt.NewFullRT(host, dht.DefaultPrefix, fullrt.DHTOption(rtCfg...))
		if err != nil {
			return nil, err
		}

		routing = fullRT
	case RoutingTableTypeDHT:
		dht, err := dht.New(ctx, host, rtCfg...)
		if err != nil {
			return nil, err
		}

		routing = dht
	}

	provider := &Provider{
		config:         config,
		network:        network.NewFromIpfsHost(host, routing),
		blockManager:   blockManager,
		retriever:      retriever,
		requestQueue:   peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{})),
		responseQueue:  peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{})),
		retrievalQueue: peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{})),
	}

	provider.network.Start(provider)

	for i := 0; i < 8; i++ {
		go provider.handleRequests()
	}

	for i := 0; i < 8; i++ {
		go provider.handleResponses()
	}

	for i := 0; i < 8; i++ {
		go provider.handleRetrievals()
	}

	return provider, nil
}

func (provider *Provider) ReceiveMessage(ctx context.Context, sender peer.ID, incoming message.BitSwapMessage) {
	var tasks []peertask.Task
	for _, entry := range incoming.Wantlist() {
		tasks = append(tasks, peertask.Task{
			Topic:    entry.Cid,
			Priority: int(entry.Priority),
			Work:     1,
			Data:     entry,
		})
	}
	provider.requestQueue.PushTasks(sender, tasks...)
}

func (provider *Provider) handleRequests() {
	ctx := context.Background()

	for {
		peerID, tasks, _ := provider.requestQueue.PopTasks(100)
		if len(tasks) == 0 {
			time.Sleep(time.Millisecond * 250)
			continue
		}

		log.Debugf("Processing %d requests for %s", len(tasks), peerID)

		for _, task := range tasks {
			entry, ok := task.Data.(message.Entry)
			if !ok {
				log.Warnf("Invalid request entry data")
				continue
			}

			provider.handleRequest(ctx, peerID, entry)
		}

		provider.requestQueue.TasksDone(peerID, tasks...)
	}
}

func (provider *Provider) handleRequest(
	ctx context.Context,
	peerID peer.ID,
	entry message.Entry,
) error {
	log := log.With("peer_id", peerID)

	// Skip blacklisted CIDs
	if provider.config.CidBlacklist[entry.Cid] {
		log.Debugf("Replying DONT_HAVE for blacklisted CID: %s", entry.Cid)
		provider.queueSendDontHave(peerID, int(entry.Priority), entry.Cid, "blacklisted_cid")
		return nil
	}

	// If it's a cancel, just remove from the queue and finish

	if entry.Cancel {
		log.Debugf("Cancelling request for %s", entry.Cid)
		provider.responseQueue.Remove(entry.Cid, peerID)
		provider.retrievalQueue.Remove(entry.Cid, peerID)
		return nil
	}

	stats.Record(ctx, metrics.BitswapRequestCount.M(1))

	// First check blockstore, immediately write to response queue if it
	// exists

	size, err := provider.blockManager.GetSize(ctx, entry.Cid)
	if err == nil {
		stats.Record(ctx, metrics.BlockstoreCacheHitCount.M(1))

		switch entry.WantType {
		case wantTypeHave:
			if size < sendBlockThreshold {
				log.Debugf("Want have for %s (under send block threshold)", entry.Cid)
				provider.queueSendBlock(peerID, int(entry.Priority), entry.Cid, size)
			} else {
				log.Debugf("Want have for %s", entry.Cid)
				provider.queueSendHave(peerID, int(entry.Priority), entry.Cid)
			}
			return nil
		case wantTypeBlock:
			log.Debugf("Want block for %s", entry.Cid)
			provider.queueSendBlock(peerID, int(entry.Priority), entry.Cid, size)
			return nil
		}
	}

	// Otherwise, write to retrieve queue regardless of whether this is a want
	// have or want block (unless retrieval is disabled)

	if provider.retriever == nil {
		provider.queueSendDontHave(peerID, int(entry.Priority), entry.Cid, "disabled_retriever")
	}

	stats.Record(ctx, metrics.BitswapRetrieverRequestCount.M(1))

	provider.retrievalQueue.PushTasks(peerID, peertask.Task{
		Topic:    entry.Cid,
		Priority: int(entry.Priority),
		Work:     1,
	})

	return nil
}

func (provider *Provider) handleResponses() {
	ctx := context.Background()

	for {
		peerID, tasks, _ := provider.responseQueue.PopTasks(targetMessageSize)
		if len(tasks) == 0 {
			time.Sleep(time.Millisecond * 250)
			continue
		}

		log.Debugf("Responding to %d requests for %s", len(tasks), peerID)

		msg := message.New(false)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			log.Debugf("Sending response for %s", cid)

			data, ok := task.Data.(ResponseData)
			if !ok {
				log.Warnf("Response task data was not a response action")
				continue
			}

			switch data.action {
			case sendHave:
				msg.AddHave(cid)
				log.Debugf("Sending have for %s", cid)

				// Response metric
				ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "HAVE"))
				stats.Record(ctx, metrics.BitswapResponseCount.M(1))
			case sendDontHave:
				msg.AddDontHave(cid)
				log.Debugf("Sending dont have for %s", cid)

				// Response metric
				ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "DONT_HAVE"), tag.Insert(metrics.BitswapDontHaveReason, data.reason))
				stats.Record(ctx, metrics.BitswapResponseCount.M(1))
			case sendBlock:
				block, err := provider.blockManager.Get(ctx, cid)
				if err != nil {
					log.Warnf("Attempted to send a block but it is not in the blockstore")
					continue
				}
				msg.AddBlock(block)
				log.Debugf("Sending block for %s", cid)

				// Response metric
				ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "BLOCK"))
				stats.Record(ctx, metrics.BitswapResponseCount.M(1))
			}
		}

		if err := provider.network.SendMessage(ctx, peerID, msg); err != nil {
			log.Warnf("Failed to send message to %s: %v", peerID, err)
			provider.responseQueue.TasksDone(peerID, tasks...)
		}

		provider.responseQueue.TasksDone(peerID, tasks...)
		log.Debugf("Sent message to %s", peerID)
	}
}

func (provider *Provider) handleRetrievals() {
	ctx := context.Background()

	for {
		peerID, tasks, _ := provider.retrievalQueue.PopTasks(1)
		if len(tasks) == 0 {
			time.Sleep(time.Millisecond * 250)
			continue
		}

		log.Debugf("Retrieval of %d CIDs queued for %s", len(tasks), peerID)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			log.Debugf("Requesting retrieval for %s", cid)

			// Try to start a new retrieval (if it's already running then no
			// need to error, just continue on to await block)
			if err := provider.retriever.Request(cid); err != nil {
				if !errors.As(err, &lassieretriever.ErrRetrievalAlreadyRunning{}) {
					if errors.Is(err, lassieretriever.ErrNoCandidates) {
						// Just do a debug print if there were no candidates because this happens a lot
						log.Debugf("No candidates for %s", cid)
					} else {
						// Otherwise, there was a real failure, print with more importance
						log.Errorf("Request for %s failed: %v", cid, err)
					}
				} else {
					log.Debugf("Retrieval already running for %s, no new one will be started", cid)
				}
			} else {
				log.Infof("Started retrieval for %s", cid)
			}
			provider.blockManager.AwaitBlock(ctx, cid, func(block blocks.Block, err error) {
				if err != nil {
					log.Debugf("Async block load failed: %s", err)
					provider.queueSendDontHave(peerID, task.Priority, block.Cid, "failed_block_load")
				} else {
					log.Debugf("Async block load completed: %s", block.Cid)
					provider.queueSendBlock(peerID, task.Priority, block.Cid, block.Size)
				}
			})
		}

		provider.retrievalQueue.TasksDone(peerID, tasks...)
	}
}

func (provider *Provider) ReceiveError(err error) {
	log.Errorf("Error receiving bitswap message: %s", err.Error())
}

func (provider *Provider) PeerConnected(peerID peer.ID) {
	log.Debugf("Peer %s connected", peerID)
}

func (provider *Provider) PeerDisconnected(peerID peer.ID) {
	log.Debugf("Peer %s disconnected", peerID)
}

func (provider *Provider) queueSendHave(peerID peer.ID, priority int, cid cid.Cid) {
	log.Debugf("Sending HAVE for %s to %s", cid, peerID)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     cid.ByteLen(),
		Data: ResponseData{
			action: sendHave,
		},
	})
}

func (provider *Provider) queueSendDontHave(peerID peer.ID, priority int, cid cid.Cid, reason string) {
	log.Debugf("Sending DONT_HAVE for %s to %s", cid, peerID)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     cid.ByteLen(),
		Data: ResponseData{
			action: sendDontHave,
			reason: reason,
		},
	})
}

func (provider *Provider) queueSendBlock(peerID peer.ID, priority int, cid cid.Cid, size int) {
	log.Debugf("Sending HAVE for %s to %s", cid, peerID)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     size,
		Data: ResponseData{
			action: sendBlock, // TODO: maybe check retrieval task for this
		},
	})
}
