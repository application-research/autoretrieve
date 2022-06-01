package bitswap

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/filecoin"
	"github.com/application-research/autoretrieve/metrics"
	"github.com/ipfs/go-bitswap/message"
	bitswap_message_pb "github.com/ipfs/go-bitswap/message/pb"
	"github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-peertaskqueue"
	"github.com/ipfs/go-peertaskqueue/peertask"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/fullrt"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"gopkg.in/yaml.v3"
)

var logger = log.Logger("autoretrieve")

const (
	wantTypeHave  = bitswap_message_pb.Message_Wantlist_Have
	wantTypeBlock = bitswap_message_pb.Message_Wantlist_Block
)

type taskQueueTopic uint

const (
	topicSendBlock taskQueueTopic = iota
	topicSendHave
	topicSendDontHave
)

const targetMessageSize = 16384

type RoutingTableType int

const (
	RoutingTableTypeDisabled RoutingTableType = iota
	RoutingTableTypeDHT
	RoutingTableTypeFull
)

func ParseRoutingTableType(routingTableType string) (RoutingTableType, error) {
	switch strings.ToLower(strings.TrimSpace(routingTableType)) {
	case RoutingTableTypeDisabled.String():
		return RoutingTableTypeDisabled, nil
	case RoutingTableTypeDHT.String():
		return RoutingTableTypeDHT, nil
	case RoutingTableTypeFull.String():
		return RoutingTableTypeFull, nil
	default:
		return 0, fmt.Errorf("unknown routing table type %s", routingTableType)
	}
}

func (routingTableType RoutingTableType) String() string {
	switch routingTableType {
	case RoutingTableTypeDisabled:
		return "disabled"
	case RoutingTableTypeDHT:
		return "dht"
	case RoutingTableTypeFull:
		return "full"
	default:
		return fmt.Sprintf("unknown routing table type %d", routingTableType)
	}
}

func (routingTableType *RoutingTableType) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	_routingTableType, err := ParseRoutingTableType(str)
	if err != nil {
		return &yaml.TypeError{
			Errors: []string{fmt.Sprintf("line %d: %v", value.Line, err.Error())},
		}
	}

	*routingTableType = _routingTableType

	return nil
}

func (routingTableType RoutingTableType) MarshalYAML() (interface{}, error) {
	return routingTableType.String(), nil
}

type ProviderConfig struct {
	CidBlacklist      map[cid.Cid]bool
	MaxBitswapWorkers uint
	RoutingTable      RoutingTableType
}

type Provider struct {
	config            ProviderConfig
	network           network.BitSwapNetwork
	blockManager      *blocks.Manager
	retriever         *filecoin.Retriever
	taskQueue         *peertaskqueue.PeerTaskQueue
	sendWorkerCount   uint
	sendWorkerCountLk sync.Mutex
}

func NewProvider(
	ctx context.Context,
	config ProviderConfig,
	host host.Host,
	datastore datastore.Batching,
	blockManager *blocks.Manager,
	retriever *filecoin.Retriever,
) (*Provider, error) {

	var routing routing.ContentRouting

	rtCfg := []dht.Option{
		dht.Datastore(datastore),
		dht.BucketSize(20),
		dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...),
	}

	switch config.RoutingTable {
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
		config:          config,
		network:         network.NewFromIpfsHost(host, routing),
		blockManager:    blockManager,
		retriever:       retriever,
		taskQueue:       peertaskqueue.New(),
		sendWorkerCount: 0,
	}

	provider.network.SetDelegate(provider)

	return provider, nil
}

func (provider *Provider) startSend() {
	provider.sendWorkerCountLk.Lock()
	defer provider.sendWorkerCountLk.Unlock()

	if provider.sendWorkerCount < provider.config.MaxBitswapWorkers && provider.taskQueue.Stats().NumPending != 0 {
		provider.sendWorkerCount++

		go func() {
			for {
				peer, tasks, _ := provider.taskQueue.PopTasks(targetMessageSize)

				if len(tasks) == 0 {
					break
				}

				msg := message.New(false)

				for _, task := range tasks {
					switch task.Topic {
					case topicSendBlock:
						msg.AddBlock(task.Data.(blocks.Block))
					case topicSendHave:
						msg.AddHave(task.Data.(cid.Cid))
					case topicSendDontHave:
						msg.AddDontHave(task.Data.(cid.Cid))
					}
				}

				provider.taskQueue.TasksDone(peer, tasks...)

				if err := provider.network.SendMessage(context.Background(), peer, msg); err != nil {
					logger.Errorf("Could not send bitswap message to %s: %v", peer, err)
				}
			}

			provider.sendWorkerCountLk.Lock()
			provider.sendWorkerCount--
			provider.sendWorkerCountLk.Unlock()
		}()
	}
}

// Upon receiving a message, provider will iterate over the requested CIDs. For
// each CID, it'll check if it's present in the blockstore. If it is, it will
// respond with that block, and if it isn't, it'll start a retrieval and
// register the block to be sent later.
func (provider *Provider) ReceiveMessage(ctx context.Context, sender peer.ID, incoming message.BitSwapMessage) {

	// For each item in the incoming wantlist, we either need to say whether we
	// have the block or not, or send the actual block.

	for _, entry := range incoming.Wantlist() {
		// Only respond to WANT_HAVE and WANT_BLOCK
		if entry.WantType != wantTypeHave && entry.WantType != wantTypeBlock {
			logger.Debugf("Other message type: %s - peer %s", entry.WantType.String(), sender)
			continue
		}

		stats.Record(ctx, metrics.BitswapRequestCount.M(1))

		// We want to skip CIDs in the blacklist, send DONT_HAVE
		if provider.config.CidBlacklist[entry.Cid] {
			logger.Debugf("Replying DONT_HAVE for blacklisted CID: %s", entry.Cid)
			provider.sendDontHave(ctx, sender, entry, "blacklisted_cid")
			continue
		}

		// First we check the local blockstore
		block, err := provider.blockManager.Get(ctx, entry.Cid)

		// If we did have the block locally, just respond with that
		if err == nil {
			switch entry.WantType {
			case wantTypeHave:
				provider.sendHave(ctx, sender, block)
				stats.Record(ctx, metrics.BlockstoreCacheHitCount.M(1))

			case wantTypeBlock:
				provider.sendBlock(ctx, sender, block)
				stats.Record(ctx, metrics.BlockstoreCacheHitCount.M(1))
			}

			continue
		}

		// If the provider is disabled, we can't really do anything, send DONT_HAVE
		if provider.retriever == nil {
			provider.sendDontHave(ctx, sender, entry, "disabled_retriever")
			continue
		}

		// Otherwise, we will need to ask for it...
		requestErr := provider.retriever.Request(entry.Cid)
		stats.Record(ctx, metrics.BitswapRetrieverRequestCount.M(1))

		// If request failed, it means there's no way we'll be able to
		// get that block at the moment, so respond with DONT_HAVE
		if requestErr != nil {
			provider.sendDontHave(ctx, sender, entry, "failed_retriever_request")
			continue
		}

		// Queue the block to be sent once we get it
		var callback func(blocks.Block)
		switch entry.WantType {
		case wantTypeHave:
			callback = func(block blocks.Block) {
				provider.sendHave(context.Background(), sender, block)
			}
		case wantTypeBlock:
			callback = func(block blocks.Block) {
				provider.sendBlock(context.Background(), sender, block)
			}
		}

		if err := provider.blockManager.GetAwait(ctx, entry.Cid, callback); err != nil {
			logger.Errorf("Error waiting for block: %v", err)
		}
	}

	provider.startSend()
}

func (provider *Provider) ReceiveError(err error) {
	logger.Errorf("Error receiving bitswap message: %v", err)
}

func (provider *Provider) PeerConnected(peer peer.ID) {}

func (provider *Provider) PeerDisconnected(peer peer.ID) {}

// Adds a BLOCK task to the task queue
func (provider *Provider) sendBlock(ctx context.Context, sender peer.ID, block blocks.Block) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "BLOCK"))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicSendBlock,
		Priority: 0,
		Work:     len(block.RawData()),
		Data:     block,
	})

	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

// Adds a DONT_HAVE task to the task queue
func (provider *Provider) sendDontHave(ctx context.Context, sender peer.ID, entry message.Entry, reason string) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "DONT_HAVE"), tag.Insert(metrics.BitswapDontHaveReason, reason))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicSendDontHave,
		Priority: 0,
		Work:     entry.Cid.ByteLen(),
		Data:     entry.Cid,
	})

	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

// Adds a HAVE task to the task queue
func (provider *Provider) sendHave(ctx context.Context, sender peer.ID, block blocks.Block) {
	ctx, _ = tag.New(ctx, tag.Insert(metrics.BitswapTopic, "HAVE"))

	provider.taskQueue.PushTasks(sender, peertask.Task{
		Topic:    topicSendHave,
		Priority: 0,
		Work:     block.Cid().ByteLen(),
		Data:     block.Cid(),
	})

	stats.Record(ctx, metrics.BitswapResponseCount.M(1))
}

// Sends either a HAVE or a block to a peer, depending on whether the peer
// requested a WANT_HAVE or WANT_BLOCK.
// func (provider *Provider) respondPositive(peer peer.ID, entry message.Entry, block blocks.Block) {
// 	msg := message.New(false)

// 	switch entry.WantType {
// 	case wantTypeHave:
// 		// If WANT_HAVE, say we have the block
// 		msg.AddHave(entry.Cid)
// 	case wantTypeBlock:
// 		// If WANT_BLOCK, make sure block is not nil, and add the block to the
// 		// response
// 		if block == nil {
// 			logger.Errorf("Cannot respond to WANT_BLOCK from %s with nil block", peer)
// 			return
// 		}
// 		msg.AddBlock(block)
// 	default:
// 		logger.Errorf("Cannot respond to %s with unknown want type %v", peer, entry.WantType)
// 		return
// 	}

// 	if err := provider.network.SendMessage(context.Background(), peer, msg); err != nil {
// 		logger.Errorf("Failed to respond to %s: %v", peer, err)
// 	}
// }

// func (provider *Provider) respondNegative(peer peer.ID, entry message.Entry) {
// 	msg := message.New(false)

// 	msg.AddDontHave(entry.Cid)

// 	if err := provider.network.SendMessage(context.Background(), peer, msg); err != nil {
// 		logger.Errorf("Failed to respond to %s: %v", peer, err)
// 	}
// }
