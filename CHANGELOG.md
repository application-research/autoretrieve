# changelog

## feat/reorg-and-config (April 2022)

- BREAKING CHANGE: rename flags 
  - `--datadir` to `--data-dir`
  - `--endpoint` to `--endpoint-url`
- BREAKING CHANGE: change config defaults
  - endpoint type from `estuary` to `indexer`
  - endpoint URL from `https://api.estuary.tech/retrieval-candidates` to `https://cid.contact`
- BREAKING CHANGE: remove flags 
  - `--max-send-workers`
  - `--per-miner-retrieval-timeout`
  - `--timeout`
  - `--miner-whitelist`
  - `--miner-blacklist`
  - `--cid-blacklist`
- BREAKING CHANGE: remove subcommands 
  - `check-miner-whitelist`
  - `check-miner-blacklist`
  - `check-cid-blacklist`
- BREAKING CHANGE: discontinue config files 
  - `miner-blacklist.txt`
  - `miner-whitelist.txt`
  - `cid-blacklist.txt`
- BREAKING CHANGE: change default data dir to `~/.autoretrieve`
- feat: create self-contained Autoretrieve and accompanying Config types, and separate from CLI code
- feat: adopt config file `config.yaml`
- feat: add subcommand `gen-config`
- feat: add subcommand `print-config`
- feat: generate config file on startup if none present
- feat: can now use either peer ID or storage provider address in config.yaml