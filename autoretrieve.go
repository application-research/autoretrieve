package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/application-research/autoretrieve/bitswap"
	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/endpoint"
	"github.com/application-research/autoretrieve/keystore"
	"github.com/application-research/autoretrieve/messagepusher"
	"github.com/application-research/autoretrieve/minerpeergetter"
	"github.com/application-research/autoretrieve/paychannelmanager"
	lassieclient "github.com/filecoin-project/lassie/pkg/client"
	lassieeventrecorder "github.com/filecoin-project/lassie/pkg/eventrecorder"
	lassieretriever "github.com/filecoin-project/lassie/pkg/retriever"
	rpcstmgr "github.com/filecoin-project/lotus/chain/stmgr/rpc"
	"github.com/filecoin-project/lotus/chain/wallet"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/paychmgr"
	"github.com/ipfs/go-cid"
	ipfsdatastore "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	flatfs "github.com/ipfs/go-ds-flatfs"
	leveldb "github.com/ipfs/go-ds-leveldb"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	peer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
)

var statFmtString = `global conn stats: 
memory: %d,
number of inbound conns: %d,
number of outbound conns: %d,
number of file descriptors: %d,
number of inbound streams: %d,
number of outbound streams: %d,
`

type Autoretrieve struct {
	host      host.Host
	retriever *lassieretriever.Retriever
	provider  *bitswap.Provider
	apiCloser func()
}

func New(cctx *cli.Context, dataDir string, cfg Config) (*Autoretrieve, error) {

	if dataDir == "" {
		return nil, fmt.Errorf("dataDir must be set")
	}

	if err := os.MkdirAll(dataDir, 0744); err != nil {
		return nil, err
	}

	// Initialize P2P host
	host, err := initHost(cctx.Context, dataDir, cfg.LogResourceManager, multiaddr.StringCast("/ip4/0.0.0.0/tcp/6746"))
	if err != nil {
		return nil, err
	}

	// Open Lotus API
	api, apiCloser, err := lcli.GetGatewayAPI(cctx)
	if err != nil {
		return nil, err
	}

	// Initialize blockstore manager
	parseShardFunc, err := flatfs.ParseShardFunc("/repo/flatfs/shard/v1/next-to-last/3")
	if err != nil {
		return nil, err
	}

	blockstoreDatastore, err := flatfs.CreateOrOpen(filepath.Join(dataDir, blockstoreSubdir), parseShardFunc, false)
	if err != nil {
		return nil, err
	}

	blockstore := blockstore.NewBlockstoreNoPrefix(blockstoreDatastore)

	// Only wrap blockstore with pruner when a prune threshold is specified
	if cfg.PruneThreshold != 0 {
		blockstore, err = blocks.NewRandomPruner(cctx.Context, blockstore, blockstoreDatastore, blocks.RandomPrunerConfig{
			Threshold:   uint64(cfg.PruneThreshold),
			PinDuration: cfg.PinDuration,
		})

		if err != nil {
			return nil, err
		}
	} else {
		logger.Warnf("No prune threshold provided, blockstore garbage collection will not be performed")
	}

	// TODO: make configurable
	blockManager := blocks.NewManager(blockstore, time.Minute*10)
	if err != nil {
		return nil, err
	}

	// Open datastore
	datastore, err := leveldb.NewDatastore(filepath.Join(dataDir, datastoreSubdir), nil)
	if err != nil {
		return nil, err
	}

	// Set up wallet
	keystore, err := keystore.OpenOrInitKeystore(filepath.Join(dataDir, walletSubdir))
	if err != nil {
		return nil, fmt.Errorf("keystore initialization failed: %w", err)
	}

	wallet, err := wallet.NewWallet(keystore)
	if err != nil {
		return nil, fmt.Errorf("wallet initialization failed: %w", err)
	}

	// Set up the PayChannelManager
	ctx, shutdown := context.WithCancel(context.Background())
	mpusher := messagepusher.NewMsgPusher(api, wallet)
	rpcStateMgr := rpcstmgr.NewRPCStateManager(api)
	pchds := namespace.Wrap(datastore, ipfsdatastore.NewKey("paych"))
	payChanStore := paychmgr.NewStore(pchds)
	payChanApi := &payChannelApiProvider{
		Gateway: api,
		wallet:  wallet,
		mp:      mpusher,
	}

	payChanMgr := paychannelmanager.NewLassiePayChannelManager(ctx, shutdown, rpcStateMgr, payChanStore, payChanApi)
	if err := payChanMgr.Start(); err != nil {
		return nil, err
	}

	minerPeerGetter := minerpeergetter.NewMinerPeerGetter(api)

	// Initialize Filecoin retriever
	var retriever *lassieretriever.Retriever
	if !cfg.DisableRetrieval {
		var ep lassieretriever.Endpoint
		switch cfg.LookupEndpointType {
		case EndpointTypeEstuary:
			logger.Infof("Using Estuary endpoint type")
			ep = endpoint.NewEstuaryEndpoint(cfg.LookupEndpointURL, minerPeerGetter)
		case EndpointTypeIndexer:
			logger.Infof("Using indexer endpoint type")
			ep = endpoint.NewIndexerEndpoint(cfg.LookupEndpointURL)
		default:
			return nil, errors.New("unrecognized endpoint type")
		}

		retrieverCfg, err := cfg.ExtractFilecoinRetrieverConfig(cctx.Context, minerPeerGetter)
		if err != nil {
			return nil, err
		}

		confirmer := func(c cid.Cid) (bool, error) {
			return blockManager.Has(cctx.Context, c)
		}

		// Instantiate client
		retrievalClient, err := lassieclient.NewClient(
			blockstore,
			datastore,
			host,
			payChanMgr,
		)
		if err != nil {
			return nil, err
		}

		if err := retrievalClient.AwaitReady(); err != nil {
			return nil, err
		}

		retriever, err = lassieretriever.NewRetriever(cctx.Context, retrieverCfg, retrievalClient, ep, confirmer)
		if err != nil {
			return nil, err
		}
		if cfg.EventRecorderEndpointURL != "" {
			logger.Infof("Reporting retrieval events to %v", cfg.EventRecorderEndpointURL)
			eventRecorderEndpointAuthorization, err := loadEventRecorderAuth(dataDirPath(cctx))
			if err != nil {
				return nil, err
			}
			retriever.RegisterListener(lassieeventrecorder.NewEventRecorder(cctx.Context, cfg.InstanceId, cfg.EventRecorderEndpointURL, eventRecorderEndpointAuthorization))
		}
	}

	// Initialize Bitswap provider
	provider, err := bitswap.NewProvider(
		cctx.Context,
		cfg.ExtractBitswapProviderConfig(cctx.Context),
		host,
		datastore,
		blockManager,
		retriever, // This will be nil if --disable-retrieval is passed
	)
	if err != nil {
		return nil, err
	}

	// Start Estuary heartbeat goroutine if an endpoint was specified
	if cfg.EstuaryURL != "" {
		_, err := url.Parse(cfg.EstuaryURL)
		if err != nil {
			return nil, fmt.Errorf("could not parse Estuary URL: %w", err)
		}

		go func() {
			logger.Infof("Starting estuary heartbeat ticker with heartbeat interval %s", cfg.HeartbeatInterval)

			ticker := time.NewTicker(cfg.HeartbeatInterval)
			if ticker == nil {
				logger.Infof("Error setting ticker")
			}
			for ; true; <-ticker.C {
				logger.Infof("Sending Estuary heartbeat message")
				interval, err := sendEstuaryHeartbeat(&cfg)
				if err != nil {
					logger.Errorf("Failed to send Estuary heartbeat message - resetting to default heartbeat interval of %s: %v", cfg.HeartbeatInterval, err)
					ticker.Reset(cfg.HeartbeatInterval)
				} else {
					logger.Infof("Next Estuary heartbeat in %s", interval)
					ticker.Reset(interval)
				}
			}
		}()
	}

	logger.Infof("Using peer ID: %s", host.ID())

	return &Autoretrieve{
		host:      host,
		retriever: retriever,
		provider:  provider,
		apiCloser: apiCloser,
	}, nil
}

func sendEstuaryHeartbeat(cfg *Config) (time.Duration, error) {

	// Set ticker

	req, err := http.NewRequest("POST", cfg.EstuaryURL+"/autoretrieve/heartbeat", bytes.NewBuffer(nil))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cfg.AdvertiseToken))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	// If we got a non-success status code, warn about the error
	if res.StatusCode/100 != 2 {
		resBytes, err := ioutil.ReadAll(res.Body)
		resString := string(resBytes)

		if err != nil {
			resString = "could not read response"
		}
		return 0, fmt.Errorf("%s", resString)
	}

	var output struct {
		Handle            string
		LastConnection    time.Time
		LastAdvertisement time.Time
		AddrInfo          peer.AddrInfo
		AdvertiseInterval string
		Error             interface{}
	}
	if err := json.NewDecoder(res.Body).Decode(&output); err != nil {
		return 0, fmt.Errorf("could not decode response: %v", err)
	}

	if output.Error != nil && output.Error != "" {
		return 0, fmt.Errorf("%v", output.Error)
	}

	advInterval, err := time.ParseDuration(output.AdvertiseInterval)
	if err != nil {
		return 0, fmt.Errorf("could not parse advertise interval: %s", err)
	}

	return advInterval / 2, nil
}

// TODO: this function should do a lot more to clean up resources and come to
// safe stop
func (autoretrieve *Autoretrieve) Close() {
	autoretrieve.apiCloser()
	autoretrieve.host.Close()
}

func loadEventRecorderAuth(dataDir string) (string, error) {
	authPath := filepath.Join(dataDir, "eventrecorderauth")
	eventRecorderAuth, err := os.ReadFile(authPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSuffix(string(eventRecorderAuth), "\n"), nil
}

func loadPeerKey(dataDir string) (crypto.PrivKey, error) {
	var peerkey crypto.PrivKey
	keyPath := filepath.Join(dataDir, "peerkey")
	keyFile, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		logger.Infof("Generating new peer key...")

		key, _, err := crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			return nil, err
		}
		peerkey = key

		data, err := crypto.MarshalPrivateKey(key)
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(keyPath, data, 0600); err != nil {
			return nil, err
		}
	} else {
		key, err := crypto.UnmarshalPrivateKey(keyFile)
		if err != nil {
			return nil, err
		}

		peerkey = key
	}

	if peerkey == nil {
		panic("sanity check: peer key is uninitialized")
	}

	return peerkey, nil
}

func initHost(ctx context.Context, dataDir string, resourceManagerStats bool, listenAddrs ...multiaddr.Multiaddr) (host.Host, error) {

	peerkey, err := loadPeerKey(dataDir)
	if err != nil {
		return nil, err
	}

	host, err := libp2p.New(libp2p.ListenAddrs(listenAddrs...), libp2p.Identity(peerkey), libp2p.ResourceManager(network.NullResourceManager))
	if err != nil {
		return nil, err
	}

	if resourceManagerStats {
		go func() {
			for range time.Tick(time.Second * 10) {
				select {
				case <-ctx.Done():
					return
				default:
				}
				err := host.Network().ResourceManager().ViewSystem(func(scope network.ResourceScope) error {
					stat := scope.Stat()
					logger.Infof(statFmtString, stat.Memory, stat.NumConnsInbound, stat.NumConnsOutbound, stat.NumFD, stat.NumStreamsInbound, stat.NumStreamsOutbound)
					return nil
				})
				if err != nil {
					logger.Errorf("unable to fetch global resource manager scope: %s", err.Error())
					return
				}
			}
		}()
	}
	return host, nil
}
