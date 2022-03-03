package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
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
	"github.com/filecoin-project/go-data-transfer/channelmonitor"
	"github.com/filecoin-project/lotus/chain/wallet"
	lcli "github.com/filecoin-project/lotus/cli"
	leveldb "github.com/ipfs/go-ds-leveldb"
	graphsync "github.com/ipfs/go-graphsync/impl"
	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
)

var logger = log.Logger("autoretrieve")

const minerBlacklistFilename = "blacklist.txt"
const minerWhitelistFilename = "whitelist.txt"
const datastoreSubdir = "datastore"
const walletSubdir = "wallet"

var flagWhitelist = &cli.StringSliceFlag{
	Name:  "whitelist",
	Usage: "Which miners to whitelist - overrides whitelist.txt",
}
var flagBlacklist = &cli.StringSliceFlag{
	Name:  "blacklist",
	Usage: "Which miners to blacklist - overrides blacklist.txt",
}

func main() {
	log.SetLogLevel("autoretrieve", "DEBUG")

	app := cli.NewApp()

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "datadir",
			Value:   "./data",
			EnvVars: []string{"AUTORETRIEVE_DATADIR"},
		},
		&cli.DurationFlag{
			Name:  "timeout",
			Value: 60 * time.Second,
			Usage: "Time to wait on a hanging retrieval before moving on, using a Go ParseDuration(...) string, e.g. 60s, 2m",
		},
		&cli.UintFlag{
			Name:  "per-miner-retrieval-limit",
			Value: 0,
			Usage: "How many active retrievals to allow per miner - set to 0 (default) for no limit",
		},
		&cli.StringFlag{
			Name:  "endpoint",
			Value: "https://api.estuary.tech/retrieval-candidates",
		},
		&cli.StringFlag{
			Name:  "endpoint-type",
			Value: "estuary",
			Usage: "type of endpoint for finding data (valid values are \"estuary\" and \"indexer\")",
		},
		&cli.UintFlag{
			Name:  "max-send-workers",
			Value: 4,
			Usage: "Max bitswap message sender worker thread count",
		},
		&cli.BoolFlag{
			Name:  "disable-retrieval",
			Usage: "Whether to disable the retriever module, for testing provider only",
		},
		&cli.BoolFlag{
			Name:  "fullrt",
			Usage: "Whether to use the full routing table instead of DHT",
		},
		&cli.BoolFlag{
			Name:  "log-resource-manager",
			Usage: "Whether to present output about the current state of the libp2p resource manager",
		},
		&cli.BoolFlag{
			Name:  "log-retrievals",
			Usage: "Wehther to present periodic output about the progress of retrievals",
		},
		flagWhitelist,
		flagBlacklist,
	}

	app.Action = run

	app.Commands = []*cli.Command{
		{
			Name:   "check-blacklist",
			Action: cmdCheckBlacklist,
			Flags:  []cli.Flag{flagBlacklist},
		},
		{
			Name:   "check-whitelist",
			Action: cmdCheckWhitelist,
			Flags:  []cli.Flag{flagWhitelist},
		},
	}

	ctx := contextWithInterruptCancel()
	if err := app.RunContext(ctx, os.Args); err != nil {
		logger.Fatalf("%w", err)
	}
}

// Creates a context that will get cancelled when the user presses Ctrl+C or
// otherwise triggers an interrupt signal.
func contextWithInterruptCancel() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)

		<-ch

		signal.Ignore(os.Interrupt)
		fmt.Printf("Interrupt detected, gracefully exiting... (interrupt again to force termination)\n")
		cancel()
	}()

	return ctx
}

// Main command entry point.
func run(cctx *cli.Context) error {
	dataDir := cctx.String("datadir")
	endpointURL := cctx.String("endpoint")
	endpointType := cctx.String("endpoint-type")
	timeout := cctx.Duration("timeout")
	maxSendWorkers := cctx.Uint("max-send-workers")
	perMinerRetrievalLimit := cctx.Uint("per-miner-retrieval-limit")

	if err := metrics.GoMetricsInjectPrometheus(); err != nil {
		logger.Warnf("Failed to inject prometheus: %v", err)
	}
	metricsInst := metrics.NewPrometheus(cctx.Context, metrics.NewBasic(&metrics.Noop{}, logger))

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/debug/stacktrace", func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 64<<20)
			for i := 0; ; i++ {
				n := runtime.Stack(buf, true)
				if n < len(buf) {
					buf = buf[:n]
					break
				}
				if len(buf) >= 1<<30 {
					// Filled 1 GB - stop there.
					break
				}
				buf = make([]byte, 2*len(buf))
			}
			_, err := w.Write(buf)
			if err != nil {
				logger.Error(err)
			}
		})
		if err := http.ListenAndServe("0.0.0.0:8080", nil); err != nil {
			logger.Errorf("Could not start prometheus endpoint server: %s", err)
		}
	}()

	// Load miner blacklist and whitelist
	minerBlacklist, err := getBlacklist(cctx)
	if err != nil {
		return err
	}

	minerWhitelist, err := getWhitelist(cctx)
	if err != nil {
		return err
	}

	// Initialize P2P host
	host, err := initHost(cctx.Context, dataDir, cctx.Bool("log-resource-manager"), multiaddr.StringCast("/ip4/0.0.0.0/tcp/6746"))
	if err != nil {
		return err
	}

	// Open Lotus API
	api, closer, err := lcli.GetGatewayAPI(cctx)
	if err != nil {
		return err
	}
	defer closer()

	// Initialize blockstore manager
	blockManager, err := blocks.NewManager(blocks.ManagerConfig{
		DataDir: dataDir,
	})
	if err != nil {
		return err
	}

	// Open datastore
	datastore, err := leveldb.NewDatastore(filepath.Join(dataDir, datastoreSubdir), nil)
	if err != nil {
		return err
	}

	// Set up FilClient

	keystore, err := keystore.OpenOrInitKeystore(filepath.Join(dataDir, walletSubdir))
	if err != nil {
		logger.Errorf("Keystore initialization failed: %v", err)
		return nil
	}

	wallet, err := wallet.NewWallet(keystore)
	if err != nil {
		logger.Errorf("Wallet initialization failed: %v", err)
	}

	walletAddr, err := wallet.GetDefault()
	if err != nil {
		walletAddr = address.Undef
	}
	metricsInst.RecordWallet(metrics.WalletInfo{
		Err:  err,
		Addr: walletAddr,
	})

	const maxTraversalLinks = 32 * (1 << 20)
	fc, err := filclient.NewClientWithConfig(&filclient.Config{
		Host:       host,
		Api:        api,
		Wallet:     wallet,
		Addr:       walletAddr,
		Blockstore: blockManager,
		Datastore:  datastore,
		DataDir:    dataDir,

		GraphsyncOpts: []graphsync.Option{
			graphsync.MaxInProgressIncomingRequests(200),
			graphsync.MaxInProgressOutgoingRequests(200),
			graphsync.MaxMemoryResponder(8 << 30),
			graphsync.MaxMemoryPerPeerResponder(32 << 20),
			graphsync.MaxInProgressIncomingRequestsPerPeer(20),
			graphsync.MessageSendRetries(2),
			graphsync.SendMessageTimeout(2 * time.Minute),
			graphsync.MaxLinksPerIncomingRequests(maxTraversalLinks),
			graphsync.MaxLinksPerOutgoingRequests(maxTraversalLinks),
		},
		ChannelMonitorConfig: channelmonitor.Config{

			AcceptTimeout:          time.Hour * 24,
			RestartDebounce:        time.Second * 10,
			RestartBackoff:         time.Second * 20,
			MaxConsecutiveRestarts: 15,
			//RestartAckTimeout:      time.Second * 30,
			CompleteTimeout: time.Minute * 40,

			// Called when a restart completes successfully
			//OnRestartComplete func(id datatransfer.ChannelID)
		},
		LogRetrievalProgressEvents: true,
	})
	if err != nil {
		logger.Errorf("FilClient initialization failed: %v", err)
	}
	// Initialize Filecoin retriever
	var retriever *filecoin.Retriever
	if !cctx.Bool("disable-retrieval") {
		minerPeerBlackList, err := toMinerPeerList(cctx.Context, fc, minerBlacklist)
		if err != nil {
			return err
		}
		minerPeerWhiteList, err := toMinerPeerList(cctx.Context, fc, minerWhitelist)
		if err != nil {
			return err
		}
		var ep filecoin.Endpoint
		switch endpointType {
		case "estuary":
			ep = endpoint.NewEstuaryEndpoint(fc, endpointURL)
		case "indexer":
			ep = endpoint.NewIndexerEndpoint(endpointURL)
		default:
			return errors.New("unrecognized endpoint type")
		}
		retriever, err = filecoin.NewRetriever(
			filecoin.RetrieverConfig{
				MinerBlacklist:         minerPeerBlackList,
				MinerWhitelist:         minerPeerWhiteList,
				RetrievalTimeout:       timeout,
				PerMinerRetrievalLimit: perMinerRetrievalLimit,
				Metrics:                metricsInst,
			},
			fc,
			ep,
			host,
			api,
			datastore,
			blockManager,
		)
		if err != nil {
			return err
		}
		if cctx.Bool("log-retrievals") {
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
					fmt.Printf("Data transfer status\n\n")
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
						fmt.Fprintf(w, dtOutput, state.OtherPeer(), state.BaseCID(), datatransfer.Statuses[state.Status()], state.Received())
					}
					w.Flush()
					fmt.Printf("\nTotal Successes: %d, Total Failures: %d\n", totalSuccesses, totalFailures)
				}
			}()
		}
	}

	// Initialize Bitswap provider
	_, err = bitswap.NewProvider(
		cctx.Context,
		bitswap.ProviderConfig{
			MaxSendWorkers: uint(maxSendWorkers),
			UseFullRT:      cctx.Bool("fullrt"),
		},
		host,
		datastore,
		blockManager,
		retriever, // This will be nil if --disable-retrieval is passed
	)
	if err != nil {
		return err
	}

	<-cctx.Context.Done()

	host.Close()

	return nil
}

var dtHeaders = "peer\tcid\tstatus\ttransferred"
var dtOutput = "%s\t%s\t%s\t%d\n"

func cmdCheckBlacklist(cctx *cli.Context) error {
	minerBlacklist, err := getBlacklist(cctx)
	if err != nil {
		return err
	}

	if len(minerBlacklist) == 0 {
		fmt.Printf("No blacklisted miners were found\n")
		return nil
	}

	for miner := range minerBlacklist {
		fmt.Printf("%s\n", miner)
	}

	return nil
}

func cmdCheckWhitelist(cctx *cli.Context) error {
	minerWhitelist, err := getWhitelist(cctx)
	if err != nil {
		return err
	}

	if len(minerWhitelist) == 0 {
		fmt.Printf("No whitelisted miners were found\n")
		return nil
	}

	for miner := range minerWhitelist {
		fmt.Printf("%s\n", miner)
	}

	return nil
}

var statFmtString = `global conn stats: 
memory: %d,
number of inbound conns: %d,
number of outbound conns: %d,
number of file descriptors: %d,
number of inbound streams: %d,
number of outbound streams: %d,
`

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

func readMinerListFile(path string) (map[address.Address]bool, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	strs := strings.Split(string(bytes), "\n")

	var blacklistArr []address.Address
	for lineNum, str := range strs {
		str = strings.TrimSpace(strings.Split(str, "#")[0])

		if str == "" {
			continue
		}

		miner, err := address.NewFromString(str)
		if err != nil {
			logger.Warnf("Skipping unparseable entry \"%v\" at line %v: %v", str, lineNum, err)
			continue
		}

		blacklistArr = append(blacklistArr, miner)
	}

	blacklist := make(map[address.Address]bool)
	for _, miner := range blacklistArr {
		blacklist[miner] = true
	}

	return blacklist, nil
}

func parseMinerListArg(cctx *cli.Context, flagName string) (map[address.Address]bool, error) {
	// Each minerStringsRaw element may contain multiple comma-separated values
	minerStringsRaw := cctx.StringSlice(flagName)

	// Split any comma-separated minerStringsRaw elements
	var minerStrings []string
	for _, raw := range minerStringsRaw {
		minerStrings = append(minerStrings, strings.Split(raw, ",")...)
	}

	miners := make(map[address.Address]bool)
	for _, ms := range minerStrings {

		miner, err := address.NewFromString(ms)
		if err != nil {
			return nil, fmt.Errorf("failed to parse miner %s: %w", ms, err)
		}

		miners[miner] = true
	}

	return miners, nil
}

// Attempts to load the passed cli flag first - if empty, loads the file instead
func getList(cctx *cli.Context, flagName string, path string) (map[address.Address]bool, error) {
	argList, err := parseMinerListArg(cctx, flagName)
	if err != nil {
		return nil, err
	}

	if len(argList) != 0 {
		return argList, nil
	}

	return readMinerListFile(path)
}

func getWhitelist(cctx *cli.Context) (map[address.Address]bool, error) {
	return getList(cctx, "whitelist", filepath.Join(cctx.String("datadir"), minerWhitelistFilename))
}

func getBlacklist(cctx *cli.Context) (map[address.Address]bool, error) {
	return getList(cctx, "blacklist", filepath.Join(cctx.String("datadir"), minerBlacklistFilename))
}

func toMinerPeerList(ctx context.Context, fc *filclient.FilClient, minerList map[address.Address]bool) (map[peer.ID]bool, error) {
	minerPeerList := make(map[peer.ID]bool, len(minerList))
	for maddr, status := range minerList {
		minerPeer, err := fc.MinerPeer(ctx, maddr)
		if err != nil {
			return nil, err
		}
		minerPeerList[minerPeer.ID] = status
	}
	return minerPeerList, nil
}
