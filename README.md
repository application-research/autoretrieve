# Autoretrieve

Autoretrieve is a standalone Graphsync-to-Bitswap proxy server, which allows IPFS clients to retrieve data which may be available on the Filecoin network but not on IPFS (such as data that has become unpinned, or data that was uploaded to a Filecoin storage provider but was never sent to an IPFS provider).

## What problem does Autoretrieve solve?

Protocol Labs develops two decentralized data transfer protocols - Bitswap and GraphSync, which back the IPFS and Filecoin networks, respectively. Although built on similar principles, these networks are fundamentally incompatible and separate - a client of one network cannot retrieve data from a provider of the other. [TODO: link more info about differences.] This raises the following issue: what if there is data that exists on Filecoin, but not on IPFS?

Autoretrieve is a "translational proxy" that allows data to be transferred from Filecoin to IPFS in an automated fashion. The existing alternatives for Autoretrieve's Filecoin-to-IPFS flow are the Boost IPFS node that providers may optionally enable, and manual transfer.

[Boost IPFS node](https://github.com/filecoin-project/boost/issues/709) is not always a feasible option for several reasons:
- Providers are not incentivized to enable this feature
- Only free retrievals are supported

In comparison, Autoretrieve:
- Is a dedicated node, and operational burden/cost does not fall on storage provider operators
- Supports paid retrievals (the Autoretrieve node operator covers the payment)

## How does Autoretrieve work?

Autoretrieve is at its core a Bitswap server. When a Bitswap request comes in, Autoretrieve queries an indexer for Filecoin storage providers that have the requested CID. The providers are sorted and retrieval is attempted sequentially until a successful retrieval is opened. As the GraphSync data lands into Autoretrieve from the storage provider, the data is streamed live back to the IPFS client.

In order for IPFS clients to be able to retrieve Filecoin data using Autoretrieve, they must be connected to Autoretrieve. Currently, Autoretrieve can be advertised to the indexer (and by extension the DHT) by Estuary. Autoretrieve does not currently have an independent way to advertise its own data.

If an Autoretrieve node is not advertised, clients may still download data from it if a connection is established either manually, or by chance while walking through the DHT searching for other providers.

## Usage

Autoretrieve uses Docker with Buildkit for build caching. Docker rebuilds are
quite fast, and it is usable for local development. Check the docker-compose
documentation for more help.

```console
$ DOCKER_BUILDKIT=1 docker-compose up
```

You may optionally set `FULLNODE_API_INFO` to a custom fullnode's WebSocket
address. The default is `FULLNODE_API_INFO=wss://api.chain.love`.

By default, config files and cache are stored at `~/.autoretrieve`. When using
docker-compose, a binding is created to this directory. This location can be
configured by setting `AUTORETRIEVE_DATA_DIR`.

Internally, the Docker volume's path on the image is `/root/.autoretrieve`. Keep
this in mind when using the Docker image directly.

## Configuration

Some CLI flags and corresponding environment variables are available for basic configuration.

For more advanced configuration, `config.yaml` may be used. It lives in the autoretrieve data directory, and will be automatically generated by running autoretrieve. It may also be manually generated using the `gen-config` subcommand.

Configurations are applied in the following order, from least to most important:
- YAML config
- Environment variables
- CLI flags

### YAML Example

```yaml
advertise-endpoint-url: # leave blank to disable, example https://api.estuary.tech/autoretrieve/heartbeat (must be registered)
advertise-endpoint-token: # leave blank to disable
lookup-endpoint-type: indexer # indexer | estuary
lookup-endpoint-url: https://cid.contact # for estuary endpoint-type: https://api.estuary.tech/retrieval-candidates
max-bitswap-workers: 1
routing-table-type: dht
prune-threshold: 1GiB # 1000000000, 1 GB, etc. Uses go-humanize for parsing. Table of valid byte sizes can be found here: https://github.com/dustin/go-humanize/blob/v1.0.0/bytes.go#L34-L62
pin-duration: 1h # 1h30m, etc.
log-resource-manager: false
log-retrieval-stats: false
disable-retrieval: false
cid-blacklist:
  - QmCID01234
  - QmCID56789
  - QmCIDabcde
miner-blacklist:
  - f01234
  - f05678
miner-whitelist:
  - f01234
default-miner-config:
  retrieval-timeout: 1m
  max-concurrent-retrievals: 1
miner-configs:
  f01234:
    retrieval-timeout: 2m30s
    max-concurrent-retrievals: 2
  f05678:
    max-concurrent-retrievals: 10
```
