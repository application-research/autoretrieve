package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/application-research/autoretrieve/bitswap"
	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/endpoint"
	"github.com/application-research/autoretrieve/filecoin"
	"github.com/application-research/autoretrieve/metrics"
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
	host host.Host
}

func New(cctx *cli.Context, cfg Config) (*Autoretrieve, error) {

	logger.Infof("Using data directory: %s", cfg.DataDir)

	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewNoop()
	}

	// Initialize P2P host
	host, err := initHost(cctx.Context, cfg.DataDir, cfg.LogResourceManager, multiaddr.StringCast("/ip4/0.0.0.0/tcp/6746"))
	if err != nil {
		return nil, err
	}

	// Open Lotus API
	api, closer, err := lcli.GetGatewayAPI(cctx)
	if err != nil {
		return nil, err
	}
	defer closer()

	// Initialize blockstore manager
	parseShardFunc, err := flatfs.ParseShardFunc("/repo/flatfs/shard/v1/next-to-last/3")
	if err != nil {
		return nil, err
	}

	blockstoreDatastore, err := flatfs.CreateOrOpen(filepath.Join(cfg.DataDir, blockstoreSubdir), parseShardFunc, false)
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

	blockManager := blocks.NewManager(blockstore)
	if err != nil {
		return nil, err
	}

	// Open datastore
	datastore, err := leveldb.NewDatastore(filepath.Join(cfg.DataDir, datastoreSubdir), nil)
	if err != nil {
		return nil, err
	}

	// Set up FilClient

	keystore, err := keystore.OpenOrInitKeystore(filepath.Join(cfg.DataDir, walletSubdir))
	if err != nil {
		return nil, fmt.Errorf("keystore initialization failed: %w", err)
	}

	wallet, err := wallet.NewWallet(keystore)
	if err != nil {
		return nil, fmt.Errorf("wallet initialization failed: %w", err)
	}

	walletAddr, err := wallet.GetDefault()
	if err != nil {
		walletAddr = address.Undef
	}
	cfg.Metrics.RecordWallet(metrics.WalletInfo{
		Err:  err,
		Addr: walletAddr,
	})

	const maxTraversalLinks = 32 * (1 << 20)
	fc, err := filclient.NewClient(
		host,
		api,
		wallet,
		walletAddr,
		blockManager,
		datastore,
		cfg.DataDir,
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
		switch cfg.EndpointType {
		case EndpointTypeEstuary:
			ep = endpoint.NewEstuaryEndpoint(fc, cfg.EndpointURL)
		case EndpointTypeIndexer:
			ep = endpoint.NewIndexerEndpoint(cfg.EndpointURL)
		default:
			return nil, errors.New("unrecognized endpoint type")
		}

		retrieverCfg, err := cfg.ExtractFilecoinRetrieverConfig(cctx.Context, fc)
		if err != nil {
			return nil, err
		}

		retriever, err = filecoin.NewRetriever(
			retrieverCfg,
			fc,
			ep,
			host,
			api,
			datastore,
			blockManager,
		)
		if err != nil {
			return nil, err
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
						if state.Status() == datatransfer.Cancelled || state.Status() == datatransfer.Failed {
							totalFailures++
							continue
						}
						if state.Status() == datatransfer.Completed {
							totalSuccesses++
							continue
						}
						fmt.Fprintf(w, dtOutput, state.OtherPeer(), state.BaseCID(), datatransfer.Statuses[state.Status()], state.Received(), state.Message())
					}
					w.Flush()
					fmt.Printf("\nTotal Successes: %d, Total Failures: %d\n\n", totalSuccesses, totalFailures)
				}
			}()
		}
	}

	// Initialize Bitswap provider
	_, err = bitswap.NewProvider(
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

	return &Autoretrieve{
		host: host,
	}, nil
}

// TODO: this function should do a lot more to clean up resources and come to
// safe stop
func (autoretrieve *Autoretrieve) Close() {
	autoretrieve.host.Close()
}

func initHost(ctx context.Context, dataDir string, resourceManagerStats bool, listenAddrs ...multiaddr.Multiaddr) (host.Host, error) {
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
