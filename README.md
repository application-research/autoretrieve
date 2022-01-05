# auto retrieve

Serve filecoin data to bitswap clients using retrieval info queried from
Estuary.

## setup

Pass `--datadir` or set `ESTUARY_AR_DATADIR` to set estuary auto retrieve's data
directory.

To blacklist or whitelist miners for auto retrieve downloads, there are two methods.

The first is to use CLI flags `--whitelist f0xxxx,f0yyyy` and `--blacklist
f0zzzz,f0wwww` 

For a persistent blacklist, create `blacklist.txt` or `whitelist.txt` in the
data directory, respectively, and populate them with a newline-separated listed
of miner strings (text after `#` will be ignored):

    f0xxxx # miner xxxx
    f0yyyy
    f0zzzz
    # f0wwww # ignored miner
    [...]