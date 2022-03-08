# auto retrieve

Serve filecoin data to bitswap clients using retrieval info queried from
Estuary.

## Setup
First we need to build autoretrieve:
```
go build
```

Then we can run it:
```
FULLNODE_API_INFO=wss://api.chain.love ./autoretrieve --datadir your-datadir
```
**Obs:** you can either pass `--datadir` or set the `ESTUARY_AR_DATADIR` environment variable to set estuary autoretrieve's data
directory.


## Blacklisting and Whitelisting miners
You can use CLI flags or create files to blacklist or whitelist miners for autoretrieve downloads.

To use CLI flags `--whitelist f0xxxx,f0yyyy` and `--blacklist f0zzzz,f0wwww`

For a persistent blacklist, create `blacklist.txt` or `whitelist.txt` in the
data directory (`your-datadir` in the example above), respectively, and populate them with a newline-separated listed
of miner strings (text after `#` will be ignored):
```
f0xxxx # miner xxxx
f0yyyy
f0zzzz
# f0wwww # ignored miner
[...]
```
## Help
```
./autoretrieve --help
NAME:
   autoretrieve - A new cli application

USAGE:
   autoretrieve [global options] command [command options] [arguments...]

COMMANDS:
   check-blacklist
   check-whitelist
   help, h          Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --datadir value                    (default: "./data") [$AUTORETRIEVE_DATADIR]
   --timeout value                    Time to wait on a hanging retrieval before moving on, using a Go ParseDuration(...) string, e.g. 60s, 2m (default: 1m0s)
   --per-miner-retrieval-limit value  How many active retrievals to allow per miner - set to 0 (default) for no limit (default: 0)
   --endpoint value                   (default: "https://api.estuary.tech/retrieval-candidates")
   --max-send-workers value           Max bitswap message sender worker thread count (default: 4)
   --disable-retrieval                Whether to disable the retriever module, for testing provider only (default: false)
   --fullrt                           Whether to use the full routing table instead of DHT (default: false)
   --whitelist value                  Which miners to whitelist - overrides whitelist.txt
   --blacklist value                  Which miners to blacklist - overrides blacklist.txt
   --help, -h                         show help (default: false)
```