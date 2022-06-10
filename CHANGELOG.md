# Changelog
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## 2022-07-10
### Added
- YAML option `advertise-endpoint-url`
- YAML option `advertise-token`

### Changed
- Use config defaults for missing config.yaml properties instead of zero-values
- Flag `--endpoint-type` to `--lookup-endpoint-type`
- Flag `--endpoint-url` to `--lookup-endpoint-url`
- YAML option `endpoint-type` to `lookup-endpoint-type`
- YAML option `endpoint-url` to `lookup-endpoint-url`
- Env var `AUTORETRIEVE_ENDPOINT_TYPE` to `AUTORETRIEVE_LOOKUP_ENDPOINT_TYPE`
- Env var `AUTORETRIEVE_ENDPOINT_URL` to `AUTORETRIEVE_LOOKUP_ENDPOINT_URL`

## 2022-07-01
### Changed
- Flag `--use-fullrt [true|false]` to `--routing-table-type [dht|full|disabled]`
- YAML option `use-fullrt: [true|false]` to `routing-table-type: [dht|full|disabled]`
- Env var `AUTORETRIEVE_USE_FULLRT=[true|false]` to `AUTORETRIEVE_ROUTING_TABLE_TYPE=[dht|full|disabled]`

## feat/reorg-and-config - 2022-04-??
### Added
- Self-contained Autoretrieve and accompanying Config types, and separate from CLI code
- Config file `config.yaml`
- Subcommand `print-config`
- Config file generation on startup if none present
- Can now use either peer ID or storage provider address in config.yaml

### Changed
- Flag `--datadir` to `--data-dir`
- Flag `--endpoint` to `--endpoint-url`
- Default endpoint type from `estuary` to `indexer`
- Default endpoint URL from `https://api.estuary.tech/retrieval-candidates` to `https://cid.contact`

### Removed
- Flag `--max-send-workers`
- Flag `--per-miner-retrieval-timeout`
- Flag `--timeout`
- Flag `--miner-whitelist`
- Flag `--miner-blacklist`
- Flag `--cid-blacklist`
- Subcommand `check-miner-whitelist`
- Subcommand `check-miner-blacklist`
- Subcommand `check-cid-blacklist`
- Config file `miner-blacklist.txt`
- Config file `miner-whitelist.txt`
- Config file `cid-blacklist.txt`
