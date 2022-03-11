# Autoretrieve

Serve filecoin data to bitswap clients using retrieval info queried from Estuary
or indexers.

## Usage

Autoretrieve uses Docker with Buildkit for build caching. Docker rebuilds are
quite fast, it is recommended to use docker-compose for both production and
development. Check the docker-compose documentation for more help.

```console
$ DOCKER_BUILDKIT=1 docker-compose up
```

You may optionally set `FULLNODE_API_INFO` to a custom fullnode's WebSocket address. The default is `FULLNODE_API_INFO=wss://api.chain.love`.

By default, config files and cache are stored in mounted Docker volume at
`./data` in the autoretrieve directory. You can configure this location by
setting `AUTORETRIEVE_DATA_DIR`.


## Blacklisting and Whitelisting miners
You can use CLI flags or config files to blacklist or whitelist miners for
autoretrieve downloads.

To use CLI flags `--whitelist f0xxxx,f0yyyy` and `--blacklist f0zzzz,f0wwww`. If
set, the CLI flags will take precedence over config files.

For a persistent blacklist or whitelist, create `blacklist.txt` and/or
`whitelist.txt` in the data directory (`your-datadir` in the example above),
respectively, and populate them with a newline-separated listed of miner strings
(text after `#` will be ignored):
```sh
f0xxxx # miner xxxx
f0yyyy
f0zzzz
# f0wwww # ignored miner
[...]
```
## Help
```console
$ autoretrieve --help

NAME:
   autoretrieve - A new cli application

USAGE:
   autoretrieve [global options] command [command options] [arguments...]

COMMANDS:
   check-blacklist  
   check-whitelist  
   help, h          Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --datadir value                    (default: "./data") [$AUTORETRIEVE_DATA_DIR]
   --timeout value                    Time to wait on a hanging retrieval before moving on, using a Go ParseDuration(...) string, e.g. 60s, 2m (default: 1m0s) [$AUTORETRIEVE_RETRIEVAL_TIMEOUT]
   --per-miner-retrieval-limit value  How many active retrievals to allow per miner - 0 indicates no limit (default: 0) [$AUTORETRIEVE_PER_MINER_RETRIEVAL_LIMIT]
   --endpoint value                   Indexer or Estuary endpoint to get retrieval candidates from (default: "https://api.estuary.tech/retrieval-candidates") [$AUTORETRIEVE_ENDPOINT]
   --endpoint-type value              Type of endpoint for finding data (valid values are "estuary" and "indexer") (default: "estuary") [$AUTORETRIEVE_ENDPOINT_TYPE]
   --max-send-workers value           Max bitswap message sender worker thread count (default: 4) [$AUTORETRIEVE_MAX_SEND_WORKERS]
   --disable-retrieval                Whether to disable the retriever module, for testing provider only (default: false) [$AUTORETRIEVE_DISABLE_RETRIEVAL]
   --fullrt                           Whether to use the full routing table instead of DHT (default: false) [$AUTORETRIEVE_USE_FULLRT]
   --log-resource-manager             Whether to present output about the current state of the libp2p resource manager (default: false) [$AUTORETRIEVE_LOG_RESOURCE_MANAGER]
   --log-retrievals                   Whether to present periodic output about the progress of retrievals (default: false) [$AUTORETRIEVE_LOG_RETRIEVALS]
   --whitelist value                  Which miners to whitelist - overrides whitelist.txt
   --blacklist value                  Which miners to blacklist - overrides blacklist.txt
   --help, -h                         show help (default: false)
```