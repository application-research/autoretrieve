package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/application-research/autoretrieve/bitswap"
	"github.com/application-research/autoretrieve/filecoin"
	"github.com/application-research/filclient"
	"github.com/dustin/go-humanize"
	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"gopkg.in/yaml.v3"
)

type EndpointType int

const (
	EndpointTypeIndexer EndpointType = iota
	EndpointTypeEstuary
)

func ParseEndpointType(endpointType string) (EndpointType, error) {
	switch strings.ToLower(strings.TrimSpace(endpointType)) {
	case EndpointTypeIndexer.String():
		return EndpointTypeIndexer, nil
	case EndpointTypeEstuary.String():
		return EndpointTypeEstuary, nil
	default:
		return 0, fmt.Errorf("unknown endpoint type %s", endpointType)
	}
}

func (endpointType EndpointType) String() string {
	switch endpointType {
	case EndpointTypeIndexer:
		return "indexer"
	case EndpointTypeEstuary:
		return "estuary"
	default:
		return fmt.Sprintf("unknown endpoint type %d", endpointType)
	}
}

func (endpointType *EndpointType) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	_endpointType, err := ParseEndpointType(str)
	if err != nil {
		return &yaml.TypeError{
			Errors: []string{fmt.Sprintf("line %d: %v", value.Line, err.Error())},
		}
	}

	*endpointType = _endpointType

	return nil
}

func (endpointType EndpointType) MarshalYAML() (interface{}, error) {
	return endpointType.String(), nil
}

type ConfigByteCount uint64

func (byteCount *ConfigByteCount) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	_byteCount, err := humanize.ParseBytes(str)
	if err != nil {
		return &yaml.TypeError{
			Errors: []string{fmt.Sprintf("line %d: %v", value.Line, err.Error())},
		}
	}

	*byteCount = ConfigByteCount(_byteCount)

	return nil
}

func (byteCount ConfigByteCount) MarshalYAML() (interface{}, error) {
	return humanize.IBytes(uint64(byteCount)), nil
}

type ConfigStorageProvider struct {
	maybeAddr address.Address
	maybePeer peer.ID
}

func NewConfigStorageProviderAddress(addr address.Address) ConfigStorageProvider {
	return ConfigStorageProvider{
		maybeAddr: addr,
	}
}

func NewConfigStorageProviderPeer(peer peer.ID) ConfigStorageProvider {
	return ConfigStorageProvider{
		maybePeer: peer,
	}
}

func (provider *ConfigStorageProvider) GetPeerID(ctx context.Context, fc *filclient.FilClient) (peer.ID, error) {
	if provider.maybeAddr == address.Undef {
		return provider.maybePeer, nil
	}

	peer, err := fc.MinerPeer(ctx, provider.maybeAddr)
	if err != nil {
		return "", fmt.Errorf("could not get peer id from address %s: %w", provider.maybeAddr, err)
	}

	return peer.ID, nil
}

func (provider *ConfigStorageProvider) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	peerID, err := peer.Decode(str)
	if err == nil {
		provider.maybePeer = peerID
		return nil
	}

	addr, err := address.NewFromString(str)
	if err == nil {
		provider.maybeAddr = addr
		return nil
	}

	return &yaml.TypeError{
		Errors: []string{fmt.Sprintf("line %d: '%s' is not a valid storage provider address or peer ID", value.Line, str)},
	}
}

func (provider ConfigStorageProvider) MarshalYAML() (interface{}, error) {
	if provider.maybeAddr != address.Undef {
		return provider.maybeAddr.String(), nil
	}

	return provider.maybePeer.String(), nil
}

type MinerConfig struct {
	RetrievalTimeout        time.Duration `yaml:"retrieval-timeout"`
	MaxConcurrentRetrievals uint          `yaml:"max-concurrent-retrievals"`
}

// All config values should be safe to leave uninitialized
type Config struct {
	EstuaryURL               string                   `yaml:"estuary-url"`
	AdvertiseInterval        time.Duration            `yaml:"advertise-interval"`
	AdvertiseToken           string                   `yaml:"advertise-token"`
	LookupEndpointType       EndpointType             `yaml:"lookup-endpoint-type"`
	LookupEndpointURL        string                   `yaml:"lookup-endpoint-url"`
	MaxBitswapWorkers        uint                     `yaml:"max-bitswap-workers"`
	RoutingTableType         bitswap.RoutingTableType `yaml:"routing-table-type"`
	PruneThreshold           ConfigByteCount          `yaml:"prune-threshold"`
	PinDuration              time.Duration            `yaml:"pin-duration"`
	LogResourceManager       bool                     `yaml:"log-resource-manager"`
	LogRetrievals            bool                     `yaml:"log-retrieval-stats"`
	DisableRetrieval         bool                     `yaml:"disable-retrieval"`
	CidBlacklist             []cid.Cid                `yaml:"cid-blacklist"`
	MinerBlacklist           []ConfigStorageProvider  `yaml:"miner-blacklist"`
	MinerWhitelist           []ConfigStorageProvider  `yaml:"miner-whitelist"`
	EventRecorderEndpointURL string                   `yaml:"event-recorder-endpoint-url"`
	InstanceId               string                   `yaml:"instance-name"`

	DefaultMinerConfig MinerConfig                           `yaml:"default-miner-config"`
	MinerConfigs       map[ConfigStorageProvider]MinerConfig `yaml:"miner-configs"`
}

// Extract relevant config points into a filecoin retriever config
func (cfg *Config) ExtractFilecoinRetrieverConfig(ctx context.Context, fc *filclient.FilClient) (filecoin.RetrieverConfig, error) {

	convertedMinerCfgs := make(map[peer.ID]filecoin.MinerConfig)
	for provider, minerCfg := range cfg.MinerConfigs {
		peer, err := provider.GetPeerID(ctx, fc)
		if err != nil {
			return filecoin.RetrieverConfig{}, err
		}

		convertedMinerCfgs[peer] = filecoin.MinerConfig(minerCfg)
	}

	blacklist, err := configStorageProviderListToPeerMap(ctx, fc, cfg.MinerBlacklist)
	if err != nil {
		return filecoin.RetrieverConfig{}, err
	}

	whitelist, err := configStorageProviderListToPeerMap(ctx, fc, cfg.MinerWhitelist)
	if err != nil {
		return filecoin.RetrieverConfig{}, err
	}

	return filecoin.RetrieverConfig{
		MinerBlacklist:     blacklist,
		MinerWhitelist:     whitelist,
		DefaultMinerConfig: filecoin.MinerConfig(cfg.DefaultMinerConfig),
		MinerConfigs:       convertedMinerCfgs,
	}, nil
}

// Extract relevant config points into a bitswap provider config
func (cfg *Config) ExtractBitswapProviderConfig(ctx context.Context) bitswap.ProviderConfig {
	return bitswap.ProviderConfig{
		CidBlacklist:      cidListToMap(ctx, cfg.CidBlacklist),
		MaxBitswapWorkers: cfg.MaxBitswapWorkers,
		RoutingTableType:  cfg.RoutingTableType,
	}
}

func LoadConfig(path string) (Config, error) {
	config := DefaultConfig()

	bytes, err := os.ReadFile(path)
	if err != nil {
		return config, err
	}

	if err := yaml.Unmarshal(bytes, &config); err != nil {
		return config, fmt.Errorf("failed to parse config: %w", err)
	}

	return config, nil
}

func DefaultConfig() Config {
	return Config{
		InstanceId:         "autoretrieve-unnamed",
		LookupEndpointType: EndpointTypeIndexer,
		LookupEndpointURL:  "https://cid.contact",
		MaxBitswapWorkers:  1,
		RoutingTableType:   bitswap.RoutingTableTypeDHT,
		PruneThreshold:     0,
		PinDuration:        1 * time.Hour,
		LogResourceManager: false,
		LogRetrievals:      false,
		DisableRetrieval:   false,
		CidBlacklist:       nil,
		MinerBlacklist:     nil,
		MinerWhitelist:     nil,

		DefaultMinerConfig: MinerConfig{
			RetrievalTimeout:        1 * time.Minute,
			MaxConcurrentRetrievals: 1,
		},
		MinerConfigs:      make(map[ConfigStorageProvider]MinerConfig),
		AdvertiseInterval: 6 * time.Hour,
	}
}

func WriteConfig(cfg Config, path string) error {
	bytes, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, bytes, 0644)
}

func cidListToMap(ctx context.Context, cidList []cid.Cid) map[cid.Cid]bool {
	cidMap := make(map[cid.Cid]bool, len(cidList))
	for _, cid := range cidList {
		cidMap[cid] = true
	}
	return cidMap
}

func configStorageProviderListToPeerMap(ctx context.Context, fc *filclient.FilClient, minerList []ConfigStorageProvider) (map[peer.ID]bool, error) {
	peerMap := make(map[peer.ID]bool, len(minerList))
	for _, provider := range minerList {
		peer, err := provider.GetPeerID(ctx, fc)
		if err != nil {
			return nil, err
		}
		peerMap[peer] = true
	}
	return peerMap, nil
}
