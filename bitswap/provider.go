package bitswap

import (
	"context"
	"errors"
	"time"

	"github.com/application-research/autoretrieve/blocks"
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
)

var log = logging.Logger("autoretrieve")

// Wantlist want type redeclarations
const (
	wantTypeHave  = bitswap_message_pb.Message_Wantlist_Have
	wantTypeBlock = bitswap_message_pb.Message_Wantlist_Block
)

// Response queue actions
type ResponseAction uint

const (
	sendHave ResponseAction = iota
	sendDontHave
	sendBlock
)

const targetMessageSize = 1 << 10

type ProviderConfig struct {
	CidBlacklist      map[cid.Cid]bool
	MaxBitswapWorkers uint
	RoutingTableType  RoutingTableType
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
	workReady      chan struct{}
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
		requestQueue:   peertaskqueue.New(),
		responseQueue:  peertaskqueue.New(),
		retrievalQueue: peertaskqueue.New(),
		workReady:      make(chan struct{}, config.MaxBitswapWorkers),
	}

	provider.network.Start(provider)

	go provider.handleRequests()
	go provider.handleResponses()
	go provider.handleRetrievals()

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

		log.Infof("Processing %d requests for %s", len(tasks), peerID)

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

	// If it's a cancel, just remove from the queue and finish

	if entry.Cancel {
		log.Infof("Cancelling request for %s", entry.Cid)
		provider.responseQueue.Remove(entry.Cid, peerID)
		return nil
	}

	// First check blockstore, immediately write to response queue if it
	// exists

	size, err := provider.blockManager.GetSize(ctx, entry.Cid)
	if err == nil {
		switch entry.WantType {
		case wantTypeHave:
			log.Infof("Want have for %s", entry.Cid)
			provider.responseQueue.PushTasks(peerID, peertask.Task{
				Topic:    entry.Cid,
				Priority: int(entry.Priority),
				Work:     entry.Cid.ByteLen(),
				Data:     sendHave,
			})
			return nil
		case wantTypeBlock:
			log.Infof("Want block for %s", entry.Cid)
			provider.responseQueue.PushTasks(peerID, peertask.Task{
				Topic:    entry.Cid,
				Priority: int(entry.Priority),
				Work:     size,
				Data:     sendBlock,
			})
			return nil
		}
	}

	// Otherwise, write to retrieve queue (regardless of whether this is a want
	// have or want block)

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

		log.Infof("Responding to %d requests for %s", len(tasks), peerID)

		msg := message.New(false)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			log.Infof("Sending response for %s", cid)

			action, ok := task.Data.(ResponseAction)
			if !ok {
				log.Warnf("Response task data was not a response action")
				continue
			}

			switch action {
			case sendHave:
				msg.AddHave(cid)
				log.Infof("Sending have for %s", cid)
			case sendDontHave:
				msg.AddDontHave(cid)
				log.Infof("Sending dont have for %s", cid)
			case sendBlock:
				block, err := provider.blockManager.Get(ctx, cid)
				if err != nil {
					log.Warnf("Attempted to send a block but it is not in the blockstore")
					continue
				}
				msg.AddBlock(block)
				log.Infof("Sending block for %s", cid)
			}
		}

		if err := provider.network.SendMessage(ctx, peerID, msg); err != nil {
			log.Warnf("Failed to send message to %s: %v", peerID, err)
			provider.responseQueue.TasksDone(peerID, tasks...)
		}

		provider.responseQueue.TasksDone(peerID, tasks...)
		log.Infof("Sent message to %s", peerID)
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

		log.Infof("Retrieval for %d CIDs queued for %s", len(tasks), peerID)

		for _, task := range tasks {
			cid, ok := task.Topic.(cid.Cid)
			if !ok {
				log.Warnf("Retrieval topic wasn't a CID")
				continue
			}

			log.Infof("Starting retrieval for %s", cid)

			// Try to start a new retrieval (if it's already running then no
			// need to error, just continue on to await block)
			if err := provider.retriever.Request(cid); err != nil && !errors.As(err, &lassieretriever.ErrRetrievalAlreadyRunning{}) {
				log.Errorf("Request for %s failed: %v", cid, err)
			}
			provider.blockManager.AwaitBlock(ctx, cid, func(block blocks.Block) {
				provider.responseQueue.PushTasks(peerID, peertask.Task{
					Topic:    cid,
					Priority: 0,
					Work:     block.Size,
					Data:     sendBlock, // TODO: maybe check retrieval task for this
				})
			})
		}

		provider.retrievalQueue.TasksDone(peerID, tasks...)
	}
}

func (provider *Provider) ReceiveError(err error) {
	log.Errorf("Error receiving bitswap message: %s", err.Error())
}

func (provider *Provider) PeerConnected(peerID peer.ID) {
	log.Infof("Peer %s connected", peerID)
}

func (provider *Provider) PeerDisconnected(peerID peer.ID) {
	log.Infof("Peer %s disconnected", peerID)
}
