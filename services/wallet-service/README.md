# wallet-service

Owns the sealed HD seed(s) for the DigiWal system. Today (phase 1):
one seed, deriving one EVM deposit address per user
(BIP44 `m/44'/60'/0'/0/{index}`). A second, separate seed for the Hot
Wallet's signing key is coming in a later phase — see
`docs/design/wallet-tiering.md` invariant #1 — deliberately never the
same seed that derives user addresses, so one compromised seed can't
expose both. See `../../docs/ARCHITECTURE.md` for how this fits into
the rest of the system and the security reasoning behind the design.

## Running it

```bash
make gen-key             # writes a fresh AES-256 key to .env (once, per environment)
make up                  # starts Postgres + Redis + this service
make init-master-wallet  # generates the master wallet — RUN EXACTLY ONCE, EVER, PER ENVIRONMENT
                          # prints the mnemonic to your terminal: write it down, it's never shown again
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
