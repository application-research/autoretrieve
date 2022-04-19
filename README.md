# Autoretrieve

Serve filecoin data to bitswap clients using retrieval info queried from Estuary
or indexers.

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
endpoint-type: indexer # indexer | estuary
endpoint-url: https://cid.contact # for estuary endpoint-type: https://api.estuary.tech/retrieval-candidates
max-bitswap-workers: 1
use-fullrt: false
prune-threshold: 1GiB # 1000000000, 1 GB, etc.
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

## Help
```console
$ autoretrieve --help
NAME:
   autoretrieve - A new cli application

USAGE:
   autoretrieve [global options] command [command options] [arguments...]

COMMANDS:
   gen-config    Generate a new config with default values
   print-config  Print detected config values as autoretrieve sees them
   help, h       Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --data-dir value        (default: "./data") [$AUTORETRIEVE_DATA_DIR]
   --endpoint-url value    Indexer or Estuary endpoint to get retrieval candidates from (default: "https://api.estuary.tech/retrieval-candidates") [$AUTORETRIEVE_ENDPOINT_URL]
   --endpoint-type value   Type of endpoint for finding data (valid values are "estuary" and "indexer") (default: "estuary") [$AUTORETRIEVE_ENDPOINT_TYPE]
   --disable-retrieval     Whether to disable the retriever module, for testing provider only (default: false) [$AUTORETRIEVE_DISABLE_RETRIEVAL]
   --use-fullrt            Whether to use the full routing table instead of DHT (default: false) [$AUTORETRIEVE_USE_FULLRT]
   --log-resource-manager  Whether to present output about the current state of the libp2p resource manager (default: false) [$AUTORETRIEVE_LOG_RESOURCE_MANAGER]
   --log-retrievals        Whether to present periodic output about the progress of retrievals (default: false) [$AUTORETRIEVE_LOG_RETRIEVALS]
   --help, -h              show help (default: false)
```
