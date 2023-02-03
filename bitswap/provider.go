package bitswap

import (
	"context"
	"errors"
	"time"

	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/metrics"
	"github.com/dustin/go-humanize"
	lassieretriever "github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/ipfs/go-bitswap/message"
	bitswap_message_pb "github.com/ipfs/go-bitswap/message/pb"
	"github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-graphsync/storeutil"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-peertaskqueue"
	"github.com/ipfs/go-peertaskqueue/peertask"
	"github.com/ipld/go-ipld-prime"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
)

var logger = log.Logger("autoretrieve")

// Wantlist want type redeclarations
const (
	wantTypeHave  = bitswap_message_pb.Message_Wantlist_Have
	wantTypeBlock = bitswap_message_pb.Message_Wantlist_Block
)

// Task queue topics
type (
	topicHave     cid.Cid
	topicDontHave cid.Cid
	topicBlock    cid.Cid
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
	linkSystem   ipld.LinkSystem
	retriever    *lassieretriever.Retriever
	taskQueue    *peertaskqueue.PeerTaskQueue
	workReady    chan struct{}
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
		config:       config,
		network:      network.NewFromIpfsHost(host, routing),
		blockManager: blockManager,
		linkSystem:   storeutil.LinkSystemForBlockstore(blockManager.Blockstore),
		retriever:    retriever,
		taskQueue:    peertaskqueue.New(),
		workReady:    make(chan struct{}, config.MaxBitswapWorkers),
	}

	provider.network.Start(provider)

	for i := uint(0); i < config.MaxBitswapWorkers; i++ {
		go provider.runWorker()
	}

	return provider, nil
}

// Upon receiving a message, provider will iterate over the requested CIDs. For
// each CID, it'll check if it's present in the blockstore. If it is, it will
// respond with that block, and if it isn't, it'll start a retrieval and
// register the block to be sent later.
func (provider *Provider) ReceiveMessage(ctx context.Context, sender peer.ID, incoming message.BitSwapMessage) {
	for _, entry := range incoming.Wantlist() {

		// Immediately, if this is a cancel message, just ignore it (TODO)
		if entry.Cancel {
			continue
		}

		stats.Record(ctx, metrics.BitswapRequestCount.M(1))

		// We want to skip CIDs in the blacklist, queue DONT_HAVE
		if provider.config.CidBlacklist[entry.Cid] {
			logger.Debugf("Replying DONT_HAVE for blacklisted CID: %s", entry.Cid)
			provider.queueDontHave(ctx, sender, entry, "blacklisted_cid")
			continue
		}

		switch entry.WantType {
		case wantTypeHave:
			// For WANT_HAVE, just confirm whether it's in the blockstore
			has, err := provider.blockManager.Has(ctx, entry.Cid)

			// If there was a problem, log and move on
			if err != nil {
				logger.Warnf("Failed to check blockstore for bitswap entry: %s", entry.Cid)
				continue
			}

			// If the block was found, queue HAVE and move on...
			if has {
				stats.Record(ctx, metrics.BlockstoreCacheHitCount.M(1))
				provider.queueHave(ctx, sender, entry)
				continue
			}

			// ...otherwise, check retrieval candidates for it
		case wantTypeBlock:
			// For WANT_BLOCK, try to get the block
			size, err := provider.blockManager.GetSize(ctx, entry.Cid)

			// If there was a problem (aside from block not found), log and move
			// on
			if err != nil && !format.IsNotFound(err) {
				logger.Warnf("Failed to get block for bitswap entry: %s", entry.Cid)
				continue
			}

			// As long as no not found error was hit, queue the block and move
			// on...
			if !format.IsNotFound(err) {
				stats.Record(ctx, metrics.BlockstoreCacheHitCount.M(1))
				provider.queueBlock(ctx, sender, entry, size)
				continue
			}

			// ...otherwise, check retrieval candidates for it
		}

		// If retriever is disabled, nothing we can do, send DONT_HAVE and move
		// on
		if provider.retriever == nil {
			provider.queueDontHave(ctx, sender, entry, "disabled_retriever")
			continue
		}

		// At this point, the blockstore did not have the requested block, so a
		// retrieval is attempted

		// Record that a retrieval is required
		stats.Record(ctx, metrics.BitswapRetrieverRequestCount.M(1))

		switch entry.WantType {
		case wantTypeHave:
			provider.retrieveForPeer(ctx, entry, sender, false)
		case wantTypeBlock:
			provider.retrieveForPeer(ctx, entry, sender, true)
		}
	}

	provider.signalWork()
}

func (provider *Provider) ReceiveError(err error) {
	logger.Errorf("Error receiving bitswap message: %s", err.Error())
}

func (provider *Provider) PeerConnected(peer peer.ID) {}

func (provider *Provider) PeerDisconnected(peer peer.ID) {}

func (provider *Provider) signalWork() {
	// Request work if any worker isn't running or do nothing otherwise
	select {
	case provider.workReady <- struct{}{}:
	default:
	}
}

func (provider *Provider) runWorker() {
	for {
		peer, tasks, pending := provider.taskQueue.PopTasks(targetMessageSize)

		// If there's nothing to do, wait for something to happen
		if len(tasks) == 0 {
			select {
			case _, ok := <-provider.workReady:
				// If the workReady channel closed, stop the worker
				if !ok {
					logger.Infof("Shutting down worker")
					return
				}
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}

		msg := message.New(false)

		for _, task := range tasks {
			switch topic := task.Topic.(type) {
			case topicHave:
				have, err := provider.blockManager.Has(context.Background(), cid.Cid(topic))
				if err != nil {
					logger.Errorf("Block load error: %s", err.Error())
					msg.AddDontHave(cid.Cid(topic))
				} else if !have {
					logger.Debugf("Had a block but lost it for want_have: %s", cid.Cid(topic))
					msg.AddDontHave(cid.Cid(topic))
				} else {
					logger.Debugf("Sending want_have to peer: %s", cid.Cid(topic))
					msg.AddHave(cid.Cid(topic))
				}
			case topicDontHave:
				msg.AddDontHave(cid.Cid(topic))
			case topicBlock:
				blk, err := provider.blockManager.Get(context.Background(), cid.Cid(topic))
				if err != nil {
					logger.Debugf("Had a block but lost it for want_block: %s (%s)", cid.Cid(topic), err.Error())
					msg.AddDontHave(cid.Cid(topic))
				} else {
					logger.Debugf("Sending want_block to peer: %s", cid.Cid(topic))
					msg.AddBlock(blk)
				}
			}
		}
		msg.SetPendingBytes(int32(pending))

		if err := provider.network.SendMessage(context.Background(), peer, msg); err != nil {
			logger.Errorf("Failed to send message %#v: %s", msg, err.Error())
		}

		provider.taskQueue.TasksDone(peer, tasks...)
	}
}

// Adds a BLOCK task to the task queue
func (provider *Provider) queueBlock(ctx context.Context, sender peer.ID, entry message.Entry, size int) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "BLOCK"))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicBlock(entry.Cid),
		Priority: int(entry.Priority),
		Work:     size,
	})

	// Record response metric
	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

// Adds a DONT_HAVE task to the task queue
func (provider *Provider) queueDontHave(ctx context.Context, sender peer.ID, entry message.Entry, reason string) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "DONT_HAVE"), tag.Insert(metrics.BitswapDontHaveReason, reason))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicDontHave(entry.Cid),
		Priority: int(entry.Priority),
		Work:     message.BlockPresenceSize(entry.Cid),
	})

	// Record response metric
	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

// Adds a HAVE task to the task queue
func (provider *Provider) queueHave(ctx context.Context, sender peer.ID, entry message.Entry) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "HAVE"))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicHave(entry.Cid),
		Priority: int(entry.Priority),
		Work:     message.BlockPresenceSize(entry.Cid),
	})

	// Record response metric
	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

func (provider *Provider) retrieveForPeer(ctx context.Context, entry message.Entry, sender peer.ID, sendBlock bool) {
	retrievalId, err := types.NewRetrievalID()
	if err != nil {
		logger.Errorf("Failed to create retrieval ID: %s", err.Error())
		return
	}

	logger.Debugf("Starting retrieval for %s (%s)", entry.Cid, retrievalId)

	// Start a background blockstore fetch with a callback to send the block
	// to the peer once it's available.
	blockCtx, blockCancel := context.WithCancel(ctx)
	if provider.blockManager.AwaitBlock(blockCtx, entry.Cid, func(block blocks.Block, err error) {
		if err != nil {
			logger.Debugf("Failed to load block: %s", err.Error())
			provider.queueDontHave(ctx, sender, entry, "failed_block_load")
		} else {
			if sendBlock {
				logger.Debugf("Successfully awaited block (want_block): %s", entry.Cid)
				provider.queueBlock(context.Background(), sender, entry, block.Size)
			} else {
				logger.Debugf("Successfully awaited block (want_have): %s", entry.Cid)
				provider.queueHave(context.Background(), sender, entry)
			}
		}
		provider.signalWork()
		blockCancel()
	}) {
		// If the block was already in the blockstore then we don't need to
		// start a retrieval.
		return
	}

	// Try to start a new retrieval (if it's already running then no
	// need to error, just continue on to await block)
	go func() {
		result, err := provider.retriever.Retrieve(ctx, types.RetrievalRequest{
			LinkSystem:  provider.linkSystem,
			RetrievalID: retrievalId,
			Cid:         entry.Cid,
		}, func(types.RetrievalEvent) {})
		if err != nil {
			if errors.Is(err, lassieretriever.ErrRetrievalAlreadyRunning) {
				logger.Debugf("Retrieval already running for %s, no new one will be started", entry.Cid)
				return // Don't send dont_have or run blockCancel(), let it async load
			} else if errors.Is(err, lassieretriever.ErrNoCandidates) {
				// Just do a debug print if there were no candidates because this happens a lot
				logger.Debugf("No candidates for %s (%s)", entry.Cid, retrievalId)
				provider.queueDontHave(ctx, sender, entry, "no_candidates")
			} else {
				// Otherwise, there was a real failure, print with more importance
				logger.Errorf("Retrieval for %s (%s) failed: %v", entry.Cid, retrievalId, err)
				provider.queueDontHave(ctx, sender, entry, "retrieval_failed")
			}
		} else {
			logger.Infof("Retrieval for %s (%s) completed (duration: %s, bytes: %s, blocks: %d)",
				entry.Cid,
				retrievalId,
				result.Duration,
				humanize.IBytes(result.Size),
				result.Blocks,
			)
		}
		blockCancel()
	}()
}
