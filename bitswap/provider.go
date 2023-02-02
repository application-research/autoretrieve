package bitswap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/metrics"
	"github.com/dustin/go-humanize"
	lassieretriever "github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-libipfs/bitswap/message"
	bitswap_message_pb "github.com/ipfs/go-libipfs/bitswap/message/pb"
	bsnet "github.com/ipfs/go-libipfs/bitswap/network"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-peertaskqueue"
	"github.com/ipfs/go-peertaskqueue/peertask"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
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
	actionSendHave ResponseAction = iota
	actionSendDontHave
	actionSendBlock
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
	network      bsnet.BitSwapNetwork
	blockManager *blocks.Manager
	retriever    *lassieretriever.Retriever

	// Incoming messages to be processed - work is 1 per message
	requestQueue           *peertaskqueue.PeerTaskQueue
	requestQueueSignalChan chan struct{}

	// Outgoing messages to be sent - work is size of messages in bytes
	responseQueue           *peertaskqueue.PeerTaskQueue
	responseQueueSignalChan chan struct{}

	// CIDs that need to be retrieved - work is 1 per CID queued
	retrievalQueue           *peertaskqueue.PeerTaskQueue
	retrievalQueueSignalChan chan struct{}
}

type overwriteTaskMerger struct{}

func (*overwriteTaskMerger) HasNewInfo(task peertask.Task, existing []*peertask.Task) bool {
	return true
}

func (*overwriteTaskMerger) Merge(task peertask.Task, existing *peertask.Task) {
	*existing = task
}

type ConnLogger struct {
	network.NoopNotifiee
}

func (ConnLogger) Connected(n network.Network, c network.Conn) {
	log.Debugf("Connection from %s with ID %s", c.RemoteMultiaddr(), c.ID())
}

func (ConnLogger) Disconnected(n network.Network, c network.Conn) {
	log.Debugf("Disconnection from %s with ID %s", c.RemoteMultiaddr(), c.ID())
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

	host.Network().Notify(&ConnLogger{})

	provider := &Provider{
		config:                   config,
		network:                  bsnet.NewFromIpfsHost(host, routing),
		blockManager:             blockManager,
		retriever:                retriever,
		requestQueue:             peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{}), peertaskqueue.IgnoreFreezing(true)),
		requestQueueSignalChan:   make(chan struct{}, 1),
		responseQueue:            peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{}), peertaskqueue.IgnoreFreezing(true)),
		responseQueueSignalChan:  make(chan struct{}, 1),
		retrievalQueue:           peertaskqueue.New(peertaskqueue.TaskMerger(&overwriteTaskMerger{}), peertaskqueue.IgnoreFreezing(true)),
		retrievalQueueSignalChan: make(chan struct{}, 1),
	}

	provider.network.Start(provider)

	for i := 0; i < int(config.RequestWorkers); i++ {
		go provider.handleRequests(ctx)
	}

	for i := 0; i < int(config.ResponseWorkers); i++ {
		go provider.handleResponses(ctx)
	}

	for i := 0; i < int(config.RetrievalWorkers); i++ {
		go provider.handleRetrievals(ctx)
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

	select {
	case provider.requestQueueSignalChan <- struct{}{}:
	default:
	}

	stats.Record(ctx, metrics.RequestQueueSize.M(int64(provider.requestQueue.Stats().NumPending)))
}

func (provider *Provider) handleRequests(ctx context.Context) {
	for ctx.Err() == nil {
		peerID, tasks, _ := provider.requestQueue.PopTasks(100)
		if len(tasks) == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond * 250):
			case <-provider.requestQueueSignalChan:
			}
			continue
		}
		stats.Record(ctx, metrics.RequestQueueSize.M(int64(provider.requestQueue.Stats().NumPending)))

		log := log.With("client", peerID)
		log.Debugf("Processing %d requests", len(tasks))

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
	log := log.With("client", peerID)
	ctx, cancel := context.WithTimeout(ctx, time.Second*15)
	defer cancel()

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
	} else if !format.IsNotFound(err) {
		log.Warnf("Failed to get block for %s: %v", entry.Cid, err)
	}

	// Otherwise, write to retrieve queue regardless of whether this is a want
	// have or want block (unless retrieval is disabled)

	if provider.retriever == nil {
		provider.queueSendDontHave(peerID, int(entry.Priority), entry.Cid, "disabled_retriever")
		return nil
	}

	stats.Record(ctx, metrics.BitswapRetrieverRequestCount.M(1))

	provider.retrievalQueue.PushTasks(peerID, peertask.Task{
		Topic:    entry.Cid,
		Priority: int(entry.Priority),
		Work:     1,
	})
	select {
	case provider.retrievalQueueSignalChan <- struct{}{}:
	default:
	}

	stats.Record(ctx, metrics.RetrievalQueueSize.M(int64(provider.retrievalQueue.Stats().NumPending)))

	return nil
}

func (provider *Provider) handleResponses(ctx context.Context) {
	for ctx.Err() == nil {
		peerID, tasks, _ := provider.responseQueue.PopTasks(targetMessageSize)
		if len(tasks) == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond * 250):
			case <-provider.responseQueueSignalChan:
			}
			continue
		}

		stats.Record(ctx, metrics.ResponseQueueSize.M(int64(provider.responseQueue.Stats().NumPending)))

		log := log.With("client", peerID)
		log.Debugf("Responding to %d requests", len(tasks))

		msg := message.New(false)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			data, ok := task.Data.(ResponseData)
			if !ok {
				log.Warnf("Response task data was not a response action")
				continue
			}

			if err := provider.handleResponse(ctx, cid, data, msg); err != nil {
				log.Warnf("Could not handle response for %s: %v", cid, err)
				continue
			}
		}

		if err := provider.network.SendMessage(ctx, peerID, msg); err != nil {
			log.Warnf("Failed to send message: %v", err)
			provider.responseQueue.TasksDone(peerID, tasks...)
		}

		provider.responseQueue.TasksDone(peerID, tasks...)
		log.Debugf("Successfully sent message")
	}
}

func (provider *Provider) handleResponse(ctx context.Context, cid cid.Cid, data ResponseData, msg message.BitSwapMessage) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*15)
	defer cancel()

	switch data.action {
	case actionSendHave:
		msg.AddHave(cid)
		log.Debugf("Sending have for %s", cid)

		// Response metric
		taggedCtx, _ := tag.New(ctx, tag.Insert(metrics.BitswapTopic, "HAVE"))
		stats.Record(taggedCtx, metrics.BitswapResponseCount.M(1))
	case actionSendDontHave:
		msg.AddDontHave(cid)
		log.Debugf("Sending dont have for %s", cid)

		// Response metric
		taggedCtx, _ := tag.New(ctx, tag.Insert(metrics.BitswapTopic, "DONT_HAVE"), tag.Insert(metrics.BitswapDontHaveReason, data.reason))
		stats.Record(taggedCtx, metrics.BitswapResponseCount.M(1))
	case actionSendBlock:
		block, err := provider.blockManager.Get(ctx, cid)
		if err != nil {
			return fmt.Errorf("attempted to send a block but it is not in the blockstore")
		}
		msg.AddBlock(block)
		log.Debugf("Sending block for %s", cid)

		// Response metric
		taggedCtx, _ := tag.New(ctx, tag.Insert(metrics.BitswapTopic, "BLOCK"))
		stats.Record(taggedCtx, metrics.BitswapResponseCount.M(1))
	}

	return nil
}

func (provider *Provider) handleRetrievals(ctx context.Context) {
	for ctx.Err() == nil {
		peerID, tasks, _ := provider.retrievalQueue.PopTasks(1)
		if len(tasks) == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Millisecond * 250):
			case <-provider.retrievalQueueSignalChan:
			}
			continue
		}

		stats.Record(ctx, metrics.RetrievalQueueSize.M(int64(provider.retrievalQueue.Stats().NumPending)))

		log := log.With("client", peerID)
		log.Debugf("Retrieval of %d CIDs queued for %s", len(tasks), peerID)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			go provider.handleRetrieval(ctx, cid, peerID, task)
		}
	}
}

func (provider *Provider) handleRetrieval(ctx context.Context, cid cid.Cid, peerID peer.ID, task *peertask.Task) {
	retrievalId, err := types.NewRetrievalID()
	if err != nil {
		log.Errorf("Failed to create retrieval ID: %s", err.Error())
	}

	log.Debugf("Starting retrieval for %s (%s)", cid, retrievalId)

	// Start a background blockstore fetch with a callback to send the block
	// to the peer once it's available.
	blockCtx, blockCancel := context.WithCancel(ctx)
	if provider.blockManager.AwaitBlock(blockCtx, cid, func(block blocks.Block, err error) {
		if err != nil {
			log.Debugf("Async block load failed: %s", err)
			provider.queueSendDontHave(peerID, task.Priority, cid, "failed_block_load")
		} else {
			log.Debugf("Async block load completed: %s", cid)
			provider.queueSendBlock(peerID, task.Priority, cid, block.Size)
		}
		blockCancel()
	}) {
		// If the block was already in the blockstore then we don't need to
		// start a retrieval.
		return
	}

	// Try to start a new retrieval (if it's already running then no
	// need to error, just continue on to await block)
	result, err := provider.retriever.Retrieve(ctx, retrievalId, cid)
	if err != nil {
		if errors.Is(err, lassieretriever.ErrRetrievalAlreadyRunning) {
			log.Debugf("Retrieval already running for %s, no new one will be started", cid)
			return // Don't send dont_have or run blockCancel(), let it async load
		} else if errors.Is(err, lassieretriever.ErrNoCandidates) {
			// Just do a debug print if there were no candidates because this happens a lot
			log.Debugf("No candidates for %s (%s)", cid, retrievalId)
			provider.queueSendDontHave(peerID, task.Priority, cid, "no_candidates")
		} else {
			// Otherwise, there was a real failure, print with more importance
			log.Errorf("Retrieval for %s (%s) failed: %v", cid, retrievalId, err)
			provider.queueSendDontHave(peerID, task.Priority, cid, "retrieval_failed")
		}
	} else {
		log.Infof("Retrieval for %s (%s) completed (duration: %s, bytes: %s, blocks: %d)", cid, retrievalId, result.Duration, humanize.IBytes(result.Size), result.Blocks)
	}

	blockCancel()
	provider.retrievalQueue.TasksDone(peerID, task)
}

func (provider *Provider) ReceiveError(err error) {
	log.Errorf("Error receiving bitswap message: %s", err.Error())
}

func (provider *Provider) PeerConnected(peerID peer.ID) {
	// TODO: too noisy: log.Debugf("Peer %s connected", peerID)
}

func (provider *Provider) PeerDisconnected(peerID peer.ID) {
	// TODO: too noisy: log.Debugf("Peer %s disconnected", peerID)
}

func (provider *Provider) queueSendHave(peerID peer.ID, priority int, cid cid.Cid) {
	log := log.With("client", peerID)
	log.Debugf("Sending HAVE for %s", cid)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     cid.ByteLen(),
		Data: ResponseData{
			action: actionSendHave,
		},
	})
	select {
	case provider.responseQueueSignalChan <- struct{}{}:
	default:
	}

	stats.Record(context.Background(), metrics.ResponseQueueSize.M(int64(provider.responseQueue.Stats().NumPending)))
}

func (provider *Provider) queueSendDontHave(peerID peer.ID, priority int, cid cid.Cid, reason string) {
	log := log.With("client", peerID)
	log.Debugf("Sending DONT_HAVE for %s", cid)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     cid.ByteLen(),
		Data: ResponseData{
			action: actionSendDontHave,
			reason: reason,
		},
	})
	select {
	case provider.responseQueueSignalChan <- struct{}{}:
	default:
	}

	stats.Record(context.Background(), metrics.ResponseQueueSize.M(int64(provider.responseQueue.Stats().NumPending)))
}

func (provider *Provider) queueSendBlock(peerID peer.ID, priority int, cid cid.Cid, size int) {
	log := log.With("client", peerID)
	log.Debugf("Sending HAVE for %s", cid)
	provider.responseQueue.PushTasks(peerID, peertask.Task{
		Topic:    cid,
		Priority: priority,
		Work:     size,
		Data: ResponseData{
			action: actionSendBlock, // TODO: maybe check retrieval task for this
		},
	})
	select {
	case provider.responseQueueSignalChan <- struct{}{}:
	default:
	}

	stats.Record(context.Background(), metrics.ResponseQueueSize.M(int64(provider.responseQueue.Stats().NumPending)))
}
