# wallet-service

Owns the sealed HD seeds for the DigiWal system, one per role
(`wallet_seeds` table): `user_derivation` derives one EVM deposit
address per user (BIP44 `m/44'/60'/0'/0/{index}`); `hot_wallet` is the
Hot Wallet's signing key (single address at index 0) — deliberately
never the same seed (`docs/design/wallet-tiering.md` invariant #1).
Schema is versioned under `migrations/` (golang-migrate) and applied on
startup.  See `../../docs/ARCHITECTURE.md` for how this fits into
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
# plain listener :8080
GET  /healthz
POST /v1/users/{userID}/deposit-address   # create if missing, return existing otherwise (idempotent)
GET  /v1/users/{userID}/deposit-address   # 404 if the user has no address yet

# mTLS listener :8443 (client cert CN checked per endpoint — docs/design/service-auth-mtls.md)
POST /v1/sign/hot-wallet                  # CN: withdrawal-service, sweeper
POST /v1/sign/user-address                # CN: sweeper only (Sweep: user key -> Hot Wallet)
```

`POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key, max_gas_price?}`
→ `{signed_raw_tx, nonce}`. Signs with the Hot Wallet only (no field can pick a key),
builds the EIP-1559 tx itself from its own RPC (`RPC_URL`; `max_gas_price` is only a
ceiling — `422` if current fees exceed it), claims the next Hot Wallet nonce atomically,
and is idempotent per `idempotency_key` (a retry returns the stored result and never
claims a second nonce; same key with a different request is `409`). `503` while a Nonce
Gap is open. `asset_id` resolves through `ASSETS_FILE` (`assets.example.json`) — a stopgap
copy of the Asset Registry, see the note in `internal/signing/assets.go`.
`POST /v1/sign/user-address` `{user_id, to_address, asset_id, amount, chain_id}` → `{signed_raw_tx}`.
The caller picks whose key signs but never the destination: `to_address` must equal the Hot
Wallet or it is `403`. Nonce is the user address's live pending nonce (RPC), no DB counter.
Run `make certs` first (creates the CA + leaf certs; the CA key lives in `~/.digiwal-ca`).

```bash
curl -X POST localhost:8080/v1/users/alice/deposit-address
# {"user_id":"alice","chain":"evm","address":"0x..."}
```

One address per user, not one per chain — an EVM address is valid on
every EVM network (Ethereum, Polygon, BSC, Arbitrum, ...), so there's
nothing chain-specific to derive separately.

## What's deliberately not here yet

No Kafka. Those belong to
`withdrawal-service` and `chain-watcher` respectively (see the phased
roadmap in `docs/ARCHITECTURE.md`) — this service's whole job in phase 1
is "derive addresses correctly and never leak the seed," and it's
easiest to verify that in isolation before wiring in the rest.

## Tests

```bash
make test
```
