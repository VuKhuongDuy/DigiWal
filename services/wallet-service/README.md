# wallet-service

Owns the sealed HD seeds for the DigiWal system, one per role
(`wallet_seeds` table): `user_derivation` derives one EVM deposit
address per user (BIP44 `m/44'/60'/0'/0/{index}`); `hot_wallet` is the
Hot Wallet's signing key (single address at index 0) — deliberately
never the same seed (`docs/design/wallet-tiering.md` invariant #1).
Schema is versioned under `migrations/` (golang-migrate) and applied on
startup. Signing endpoints are not implemented yet (tasks 0.3+). See `../../docs/ARCHITECTURE.md` for how this fits into
the rest of the system and the security reasoning behind the design.

## Running it

```bash
make gen-key             # writes a fresh AES-256 key to .env (once, per environment)
make up                  # starts Postgres + Redis + this service
make init-seeds         # generates the user_derivation + hot_wallet seeds — RUN EXACTLY ONCE, EVER, PER ENVIRONMENT
                          # (or one at a time: make init-seed ROLE=hot_wallet)
                          # prints each mnemonic to your terminal: write it down, it's never shown again
make down
```

## API

```
GET  /healthz
POST /v1/users/{userID}/deposit-address   # create if missing, return existing otherwise (idempotent)
GET  /v1/users/{userID}/deposit-address   # 404 if the user has no address yet
```

```bash
curl -X POST localhost:8080/v1/users/alice/deposit-address
# {"user_id":"alice","chain":"evm","address":"0x..."}
```

One address per user, not one per chain — an EVM address is valid on
every EVM network (Ethereum, Polygon, BSC, Arbitrum, ...), so there's
nothing chain-specific to derive separately.

## What's deliberately not here yet

No signing endpoint, no chain connection, no Kafka. Those belong to
`withdrawal-service` and `chain-watcher` respectively (see the phased
roadmap in `docs/ARCHITECTURE.md`) — this service's whole job in phase 1
is "derive addresses correctly and never leak the seed," and it's
easiest to verify that in isolation before wiring in the rest.

## Tests

```bash
make test
```
