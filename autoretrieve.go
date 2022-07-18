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
	"text/tabwriter"
	"time"

	"github.com/application-research/autoretrieve/bitswap"
	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/endpoint"
	"github.com/application-research/autoretrieve/filecoin"
	"github.com/application-research/autoretrieve/filecoin/eventrecorder"
	"github.com/application-research/filclient"
	"github.com/application-research/filclient/keystore"
	"github.com/filecoin-project/go-address"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/lotus/chain/wallet"
	lcli "github.com/filecoin-project/lotus/cli"
	flatfs "github.com/ipfs/go-ds-flatfs"
	leveldb "github.com/ipfs/go-ds-leveldb"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
)

var dtHeaders = "peer\tcid\tstatus\ttransferred\tmessage"
var dtOutput = "%s\t%s\t%s\t%d\t%s\n"

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
	retriever *filecoin.Retriever
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

	// Set up FilClient

	keystore, err := keystore.OpenOrInitKeystore(filepath.Join(dataDir, walletSubdir))
	if err != nil {
		return nil, fmt.Errorf("keystore initialization failed: %w", err)
	}

	wallet, err := wallet.NewWallet(keystore)
	if err != nil {
		return nil, fmt.Errorf("wallet initialization failed: %w", err)
	}

	walletAddr, err := wallet.GetDefault()
	if err != nil {
		logger.Warnf("Could not load any default wallet address, only free retrievals will be attempted: %v", err)
		walletAddr = address.Undef
	} else {
		logger.Infof("Using default wallet address %s", walletAddr)
	}

	fc, err := filclient.NewClient(
		host,
		api,
		wallet,
		walletAddr,
		blockManager,
		datastore,
		dataDir,
		func(cfg *filclient.Config) {
			cfg.LogRetrievalProgressEvents = true
		},
	)
	if err != nil {
		logger.Errorf("FilClient initialization failed: %v", err)
	}
	// Initialize Filecoin retriever
	var retriever *filecoin.Retriever
	if !cfg.DisableRetrieval {
		var ep filecoin.Endpoint
		switch cfg.LookupEndpointType {
		case EndpointTypeEstuary:
			logger.Infof("Using Estuary endpoint type")
			ep = endpoint.NewEstuaryEndpoint(fc, cfg.LookupEndpointURL)
		case EndpointTypeIndexer:
			logger.Infof("Using indexer endpoint type")
			ep = endpoint.NewIndexerEndpoint(cfg.LookupEndpointURL)
		default:
			return nil, errors.New("unrecognized endpoint type")
		}

		retrieverCfg, err := cfg.ExtractFilecoinRetrieverConfig(cctx.Context, fc)
		if err != nil {
			return nil, err
		}

		retriever, err = filecoin.NewRetriever(retrieverCfg, fc, ep)
		if err != nil {
			return nil, err
		}
		if cfg.EventRecorderEndpointURL != "" {
			logger.Infof("Reporting retrieval events to %v", cfg.EventRecorderEndpointURL)
			retriever.RegisterListener(eventrecorder.NewEventRecorder(cfg.InstanceId, cfg.EventRecorderEndpointURL))
		}
		if cfg.LogRetrievals {
			w := tabwriter.NewWriter(os.Stdout, 5, 0, 3, ' ', 0)
			go func() {
				for range time.Tick(time.Second * 10) {
					select {
					case <-cctx.Context.Done():
						return
					default:
					}
					transfers, err := fc.TransfersInProgress(cctx.Context)
					if err != nil {
						logger.Errorf("unable to fetch transfers in progress: %s", err.Error())
						return
					}
					totalSuccesses := 0
					totalFailures := 0
					fmt.Printf("\nData transfer status\n\n")
					fmt.Fprintln(w, dtHeaders)
					for _, state := range transfers {
						if state.Status == datatransfer.Cancelled || state.Status == datatransfer.Failed {
							totalFailures++
							continue
						}
						if state.Status == datatransfer.Completed {
							totalSuccesses++
							continue
						}
						fmt.Fprintf(w, dtOutput, state.RemotePeer, state.BaseCid, datatransfer.Statuses[state.Status], state.Received, state.Message)
					}
					w.Flush()
					fmt.Printf("\nTotal Successes: %d, Total Failures: %d\n\n", totalSuccesses, totalFailures)
				}
			}()
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
			logger.Infof("Starting estuary heartbeat ticker with AdvertiseInterval=%s", cfg.AdvertiseInterval)
			ticker := time.NewTicker(cfg.AdvertiseInterval / 2)
			if ticker == nil {
				logger.Infof("Error setting ticker")
			}
			for ; true; <-ticker.C {
				logger.Infof("Sending Estuary heartbeat message")
				err = sendEstuaryHeartbeat(&cfg, ticker)
				if err != nil {
					logger.Errorf("Failed to send Estuary heartbeat message: %s", err)
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

func sendEstuaryHeartbeat(cfg *Config, ticker *time.Ticker) error {
	req, err := http.NewRequest("POST", cfg.EstuaryURL+"/autoretrieve/heartbeat", bytes.NewBuffer(nil))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", cfg.AdvertiseToken))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// If we got a non-success status code, warn about the error
	if res.StatusCode/100 != 2 {
		resBytes, err := ioutil.ReadAll(res.Body)
		resString := string(resBytes)

		if err != nil {
			resString = "could not read response"
		}
		return fmt.Errorf("%s", resString)
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

		return fmt.Errorf("could not decode response: %v", err)
	}

	if output.Error != nil && output.Error != "" {
		return fmt.Errorf("%v", output.Error)
	}

	advInterval, err := time.ParseDuration(output.AdvertiseInterval)
	if err != nil {
		return fmt.Errorf("could not parse AdvertiseInterval: %s", err)
	}
	if advInterval != cfg.AdvertiseInterval { // update advertisement interval
		ticker.Reset(advInterval)
	}
	logger.Infof("Next Estuary heartbeat in %s", advInterval/2)
	return nil
}

// TODO: this function should do a lot more to clean up resources and come to
// safe stop
func (autoretrieve *Autoretrieve) Close() {
	autoretrieve.apiCloser()
	autoretrieve.host.Close()
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
