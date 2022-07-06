package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"github.com/application-research/autoretrieve/bitswap"
	"github.com/application-research/autoretrieve/blocks"
	"github.com/application-research/autoretrieve/metrics"
	"github.com/dustin/go-humanize"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	flatfs "github.com/ipfs/go-ds-flatfs"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	format "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-merkledag"
	crypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
)

var logger = log.Logger("autoretrieve")

// Relative to data dir
const (
	datastoreSubdir  = "datastore"
	walletSubdir     = "wallet"
	blockstoreSubdir = "blockstore"
	configPath       = "config.yaml"
)

func main() {
	log.SetLogLevel("autoretrieve", "DEBUG")

	app := cli.NewApp()

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "data-dir",
			EnvVars: []string{"AUTORETRIEVE_DATA_DIR"},
		},
		&cli.StringFlag{
			Name:    "lookup-endpoint-url",
			Usage:   "Indexer or Estuary endpoint to get retrieval candidates from",
			EnvVars: []string{"AUTORETRIEVE_LOOKUP_ENDPOINT_URL"},
		},
		&cli.StringFlag{
			Name:    "lookup-endpoint-type",
			Usage:   "Type of endpoint for finding data (valid values are \"estuary\" and \"indexer\")",
			EnvVars: []string{"AUTORETRIEVE_LOOKUP_ENDPOINT_TYPE"},
		},
		&cli.BoolFlag{
			Name:    "disable-retrieval",
			Usage:   "Whether to disable the retriever module, for testing provider only",
			EnvVars: []string{"AUTORETRIEVE_DISABLE_RETRIEVAL"},
		},
		&cli.StringFlag{
			Name:    "routing-table-type",
			Usage:   "[dht|fullrt|disabled]",
			EnvVars: []string{"AUTORETRIEVE_ROUTING_TABLE_TYPE"},
		},
		&cli.BoolFlag{
			Name:    "log-resource-manager",
			Usage:   "Whether to present output about the current state of the libp2p resource manager",
			EnvVars: []string{"AUTORETRIEVE_LOG_RESOURCE_MANAGER"},
		},
		&cli.BoolFlag{
			Name:    "log-retrievals",
			Usage:   "Whether to present periodic output about the progress of retrievals",
			EnvVars: []string{"AUTORETRIEVE_LOG_RETRIEVALS"},
		},
	}

	app.Action = cmd

	app.Commands = []*cli.Command{
		{
			Name:   "gen-config",
			Action: cmdGenConfig,
			Usage:  "Generate a new config with default values",
		},
		{
			Name:   "print-config",
			Action: cmdPrintConfig,
			Usage:  "Print detected config values as autoretrieve sees them",
		},
		{
			Name:   "check-cid",
			Action: cmdTestBlockstore,
			Usage:  "Takes a CID argument and tries walking the DAG using the local blockstore",
		},
		{
			Name:      "register-estuary",
			Action:    cmdRegisterEstuary,
			Usage:     "Automatically registers this instance with Estuary for content advertisement and updates the config as necessary",
			ArgsUsage: "<endpoint url> <admin token>",
		},
	}

	ctx := contextWithInterruptCancel()
	if err := app.RunContext(ctx, os.Args); err != nil {
		fmt.Printf("%v\n", err)
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
func cmd(ctx *cli.Context) error {

	cfg, err := getFullConfig(ctx)
	if err != nil {
		return err
	}

	go func() {
		http.Handle("/metrics", metrics.PrometheusHandler())
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

	autoretrieve, err := New(ctx, dataDirPath(ctx), cfg)
	if err != nil {
		return err
	}

	<-ctx.Context.Done()

	autoretrieve.Close()

	return nil
}

func cmdTestBlockstore(ctx *cli.Context) error {
	// Initialize blockstore manager
	parseShardFunc, err := flatfs.ParseShardFunc("/repo/flatfs/shard/v1/next-to-last/3")
	if err != nil {
		return err
	}

	blockstoreDatastore, err := flatfs.CreateOrOpen(filepath.Join(dataDirPath(ctx), blockstoreSubdir), parseShardFunc, false)
	if err != nil {
		return err
	}

	blockstore := blockstore.NewBlockstoreNoPrefix(blockstoreDatastore)

	blockManager := blocks.NewManager(blockstore, 0)
	if err != nil {
		return err
	}

	bs := blockservice.New(blockManager, offline.Exchange(blockManager))
	ds := merkledag.NewDAGService(bs)

	cset := cid.NewSet()
	c, err := cid.Parse(ctx.Args().First())
	if err != nil {
		return err
	}

	var size int
	var count int
	complete := true

	if err := merkledag.Walk(ctx.Context, func(ctx context.Context, c cid.Cid) ([]*format.Link, error) {
		node, err := ds.Get(ctx, c)
		if err != nil {
			return nil, err
		}

		if c.Type() == cid.Raw {
			return nil, nil
		}

		return node.Links(), nil
	}, c, func(c cid.Cid) bool {
		blockSize, err := blockManager.GetSize(ctx.Context, c)
		if err != nil {
			fmt.Printf("Error getting block size: %v\n", err)
			complete = false
			return false
		}

		size += blockSize
		count++

		return cset.Visit(c)
	}); err != nil {
		fmt.Printf("Failed: %v\n", err)
	}

	fmt.Printf("Got size %s from %d blocks\n", humanize.IBytes(uint64(size)), count)

	if complete {
		fmt.Printf("Tree is complete\n")
	} else {
		fmt.Printf("Tree is incomplete\n")
	}

	return nil
}

func cmdGenConfig(ctx *cli.Context) error {
	cfg := DefaultConfig()
	if err := applyConfigCLIOverrides(ctx, &cfg); err != nil {
		return err
	}

	cfgPath := fullConfigPath(ctx)
	fmt.Printf("Writing config to '%s'\n", cfgPath)
	WriteConfig(cfg, cfgPath)

	return nil
}

func cmdPrintConfig(ctx *cli.Context) error {
	cfg, err := getFullConfig(ctx)
	if err != nil {
		return err
	}

	bytes, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n", string(bytes))

	return nil
}

func cmdRegisterEstuary(ctx *cli.Context) error {
	endpointURL := ctx.Args().Get(0)
	token := ctx.Args().Get(1)

	if endpointURL == "" {
		return fmt.Errorf("an Estuary endpoint URL is required (first argument)")
	}

	if token == "" {
		return fmt.Errorf("an Estuary admin token is required (second argument)")
	}

	// Get public IP address

	publicIPRes, err := http.Get("http://ip-api.com/json")
	if err != nil {
		return fmt.Errorf("could not get public ip: %v", err)
	}
	defer publicIPRes.Body.Close()

	var ipInfo struct {
		Query string
	}

	if err := json.NewDecoder(publicIPRes.Body).Decode(&ipInfo); err != nil {
		return fmt.Errorf("could not get public ip: %v", err)
	}

	fmt.Printf("Using IP: %s\n", ipInfo.Query)

	// Get peer key

	peerkey, err := loadPeerKey(dataDirPath(ctx))
	if err != nil {
		return fmt.Errorf("couldn't get peer key: %v", err)
	}

	// peerkeyPubProto, err := crypto.PublicKeyToProto(peerkey.GetPublic())
	// if err != nil {
	// 	return fmt.Errorf("couldn't get peer public key: %v", err)
	// }

	peerkeyPubBytes, err := crypto.MarshalPublicKey(peerkey.GetPublic())
	if err != nil {
		return fmt.Errorf("couldn't marshal peer public key: %v", err)
	}
	peerkeyPub := crypto.ConfigEncodeKey(peerkeyPubBytes)

	peerID, err := peer.IDFromPrivateKey(peerkey)
	if err != nil {
		return fmt.Errorf("couldn't get peer ID from key: %v", err)
	}

	// fmt.Printf("Using peer ID: %s\n", peer)
	fmt.Printf("Using public key: %s\n", peerkeyPub)

	// Do registration
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("addresses", fmt.Sprintf("/ip4/%s/tcp/6746/p2p/%s", ipInfo.Query, peerID))
	writer.WriteField("pubKey", peerkeyPub)
	writer.Close()

	req, err := http.NewRequest(
		"POST",
		endpointURL+"/admin/autoretrieve/init",
		body,
	)

	if err != nil {
		return fmt.Errorf("building registration request failed: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("registration request failed: %v", err)
	}
	defer res.Body.Close()

	var output struct {
		Handle            string
		Token             string
		LastConnection    string
		AdvertiseInterval string
		AddrInfo          peer.AddrInfo
		Error             interface{}
	}
	// outputStr, err := ioutil.ReadAll(res.Body)
	if err := json.NewDecoder(res.Body).Decode(&output); err != nil {
		return fmt.Errorf("couldn't decode response: %v", err)
	}

	if output.Error != nil && output.Error != "" {
		return fmt.Errorf("registration failed: %v\n", output.Error)
	}

	cfg, err := LoadConfig(fullConfigPath(ctx))
	cfg.EstuaryURL = endpointURL
	cfg.AdvertiseToken = output.Token

	advInterval, err := time.ParseDuration(output.AdvertiseInterval)
	if err != nil {
		return fmt.Errorf("could not parse AdvertiseInterval: %s\n", err)
	}
	cfg.AdvertiseInterval = advInterval

	if err := WriteConfig(cfg, fullConfigPath(ctx)); err != nil {
		return fmt.Errorf("failed to write config: %v", err)
	}

	logger.Infof("Successfully registered")

	return nil
}

func getFullConfig(ctx *cli.Context) (Config, error) {
	cfgPath := fullConfigPath(ctx)
	fmt.Printf("Reading config from '%s'\n", cfgPath)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("NOTE: no config file found, using defaults; run autoretrieve or use the gen-config subcommand to generate one\n-----\n")
			cfg = DefaultConfig()
		} else {
			return Config{}, err
		}
	}

	if err := applyConfigCLIOverrides(ctx, &cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func dataDirPath(ctx *cli.Context) string {
	dataDir := ctx.String("data-dir")

	if dataDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			homeDir = "./"
		}

		dataDir = path.Join(homeDir, "/.autoretrieve")
	}

	return dataDir
}

func fullConfigPath(ctx *cli.Context) string {
	return path.Join(dataDirPath(ctx), configPath)
}

// Modifies a config in-place using args passed in through CLI
func applyConfigCLIOverrides(ctx *cli.Context, cfg *Config) error {
	if ctx.IsSet("lookup-endpoint-type") {
		lookupEndpointType, err := ParseEndpointType(ctx.String("lookup-endpoint-type"))
		if err != nil {
			return err
		}

		cfg.LookupEndpointType = lookupEndpointType
	}

	if ctx.IsSet("lookup-endpoint-url") {
		cfg.LookupEndpointURL = ctx.String("lookup-endpoint-url")
	}

	if ctx.IsSet("routing-table-type") {
		routingTableType, err := bitswap.ParseRoutingTableType(ctx.String("routing-table-type"))
		if err != nil {
			return err
		}

		cfg.RoutingTableType = routingTableType
	}

	if ctx.IsSet("disable-retrieval") {
		cfg.DisableRetrieval = ctx.Bool("disable-retrieval")
	}

	if ctx.IsSet("log-resource-manager") {
		cfg.LogResourceManager = ctx.Bool("log-resource-manager")
	}

	if ctx.IsSet("log-retrievals") {
		cfg.LogRetrievals = ctx.Bool("log-retrievals")
	}

	return nil
}
