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
make init-seeds         # generates the (dev, software) hot_wallet seed — RUN EXACTLY ONCE, EVER, PER ENVIRONMENT
                          # prints the mnemonic to your terminal: write it down, it's never shown again
                          # (the user_derivation seed is not made here any more: key-admin init-seed, held only by sweep-signer)
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
GET  /v1/sign/hot-wallet/{key}            # CN: withdrawal-service, sweeper; own keys only -> signature + replacements, else 404
POST /v1/sign/hot-wallet/{key}/void       # CN: withdrawal-service only -> {"status":"voided"} | {"status":"already_signed",...}
POST /v1/sign/hot-wallet/{key}/replace    # CN: withdrawal-service, sweeper; own keys only {replacement_seq, max_gas_price?} -> Fee Bump
GET  /v1/addresses/{address}              # CN: chain-watcher only -> {user_id, chain} or 404
GET  /v1/wallet/hot-address               # CN: withdrawal-service, sweeper, chain-watcher -> {address}; 503 without a hot_wallet seed
GET  /v1/wallet/system-addresses          # CN: withdrawal-service, auditor (dev) -> {chain_id, hot, warm, cold?}; 503 until the Hot Wallet is configured
POST /v1/wallet/nonce-gap                 # CN: withdrawal-service only {chain_id, from_nonce} -> freeze Hot Wallet signing
```

`POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key, max_gas_price?}`
→ `{signed_raw_tx, nonce}`. Signs with the Hot Wallet only (no field can pick a key),
builds the EIP-1559 tx itself from its own RPC (`RPC_URL`; `max_gas_price` is only a
ceiling — `422` if current fees exceed it), claims the next Hot Wallet nonce atomically,
and is idempotent per `idempotency_key` (a retry returns the stored result and never
claims a second nonce; same key with a different request is `409`). `503` while a Nonce
Gap is open. `asset_id` resolves through the one canonical Asset Registry, `infra/assets.yaml`
(`ASSETS_FILE`), mounted identically into every service that reads it.
`GET /v1/addresses/{address}` is chain-watcher's cache-miss fallback for resolving a deposit's
owner (`deposit-flow.md`, Address Resolution): any casing in, `200 {"user_id","chain"}`, `404` only
when the address is genuinely not ours (any other failure is a 5xx, so the caller retries instead of
dropping the transfer). Its fast path is the Redis hash `wallet:address_owners` (field = address
LOWER-CASED, value = user_id), written when an address is issued. It replaces the old
`wallet:deposit_addresses` set, which couldn't carry the owner; addresses issued before this change
are simply not in the hash and resolve through the endpoint.

`GET /v1/wallet/hot-address` returns the address wallet-service itself derived for the Hot Wallet
(`deposit-flow.md`, Hot Wallet Address Discovery, invariants #15 and #17). Callers ask once at startup and
cache it rather than configure it. chain-watcher uses it only to skip Hot Wallet Outflow and still cannot
reach either `/v1/sign/*` endpoint.

`GET /v1/wallet/system-addresses` adds the **Warm** Wallet address (`wallet-tiering.md` invariant #9).
The Warm seed is never in this service, so the address comes from the operator's Treasury file,
`infra/treasury.yaml` (`TREASURY_FILE`, mounted into wallet-service only): `chains: [{chain_id,
warm_address}]`. The file is **required** in server mode: wallet-service refuses to start if it is
missing or unparseable, lists no chains, has a non-positive or duplicate chain id, or a malformed or
zero address; and, once the Hot Wallet is configured, if there is no entry for its chain or the Warm
address equals the Hot Wallet's. withdrawal-service uses it to refuse withdrawals to the Hot/Warm
Wallet (`withdrawal-flow.md` #18) and later as its Treasury Sweep destination. It sits beside
`hot-address`, which is unchanged. A chain entry may also carry an optional public `cold_address` (informational: it
restricts nothing, withdrawals to Cold stay allowed). If present it is validated like the Warm address, must differ from
the Hot and Warm addresses (else startup fails), and is added to the answer as `cold`; without one the answer is unchanged.
`CN=auditor` (the dev auditor) may read this endpoint and nothing else. To change the Warm address: edit the file, restart wallet-service,
then withdrawal-service.

**Never strand a nonce** (task 3.13, `withdrawal-flow.md` invariants #21-#23). A signed Hot Wallet tx that
never lands blocks every later one, so a key can be looked up, voided, re-signed with higher fees, or
cancelled. Every call is scoped to the caller's own keys by the verified certificate CN (`caller_cn`,
stored at sign time): another caller's key is `404` (GET) or `409` (void, replace), and rows signed before
the migration are nobody's.
- `POST .../void` is the only safe basis for Releasing a request whose sign reply may have been lost. Under
  the same `hot_wallet_nonces` lock signing takes, it either returns the signature the key already has
  (`already_signed`, nothing changes) or writes a tombstone, after which `POST /v1/sign/hot-wallet` for that
  key answers `410` and allocates nothing. Idempotent. (A plain `GET` is not enough: a sign call in flight
  can land after it said `404`.)
- `POST .../replace` `{replacement_seq, max_gas_price?}` is a **Fee Bump**: the same tx (nonce, `to`, value,
  data, gas) rebuilt from the stored original with fees `max(network, previous × 1.125, rounded up)` on both
  `maxFeePerGas` and `maxPriorityFeePerGas`. The request has no field that could change the destination or
  amount. Capped by `max_gas_price` and by a hard ceiling, `HOT_WALLET_MAX_FEE_PER_GAS_WEI` (default 50 gwei,
  Decided): above it `422`, nothing signed. The same `replacement_seq` replays the stored replacement, the
  next one creates it, any other is `409`; a replacement allocates no nonce; `503` under a Nonce Gap freeze
  that covers the nonce; `409` once a Nonce Filler is the latest version.
- **Nonce Filler**, operator only, no network endpoint (so no service certificate can cancel a payout):
  `docker compose exec wallet-service service hot-wallet cancel --key withdrawal:<withdrawal_id>` signs and
  broadcasts a 0-value Hot → Hot tx at the key's nonce with higher fees, recorded as a `cancel` replacement.
  It consumes the nonce, so the withdrawal can never land and `next_nonce` needs no reset. Running it again
  re-sends the same filler. It works under a Nonce Gap freeze (that is how a gap is repaired). Once it is
  mined, withdrawal-service's Reaper proves the nonce consumed and Releases the Lock. A filler for a nonce
  with no key (a gap hole) is not built yet.

`POST /v1/wallet/nonce-gap` `{chain_id, from_nonce}` (`withdrawal-flow.md` invariants #7, #19) is how
withdrawal-service reports a Nonce Gap: from then on no new Hot Wallet nonce is allocated for any caller
(`POST /v1/sign/hot-wallet` answers `503`; a replay of an already-signed key still works). It returns the
effective freeze `{chain_id, frozen_from_nonce, frozen_at}`: the lowest nonce ever reported wins, so a
repeat is a no-op and the caller just retries until `200`. `404` if this chain never allocated a nonce,
`422` if `from_nonce` was never allocated. **There is no endpoint to lift a freeze** — an operator does it
in the database after resolving every disputed withdrawal (see withdrawal-service's README):

```sql
UPDATE hot_wallet_nonces SET frozen_from_nonce = NULL, frozen_at = NULL WHERE chain_id = <chain>;
```

`POST /v1/sign/user-address` is **gone from this service** (M3b): it is `sweep-signer`'s, with the same request and response (see `../sweep-signer/README.md`).
Run `make certs` first (creates the CA + leaf certs; the CA key lives in `~/.digiwal-ca`).

```bash
curl -X POST localhost:8080/v1/users/alice/deposit-address
# {"user_id":"alice","chain":"evm","address":"0x..."}
```

One address per user, not one per chain — an EVM address is valid on
every EVM network (Ethereum, Polygon, BSC, Arbitrum, ...), so there's
nothing chain-specific to derive separately.

## Hot Key backends (`SIGNER_BACKEND`)

`software` (default, dev profile): the Hot Wallet is index 0 of the sealed `hot_wallet` seed. `pkcs11`: the Hot Wallet is
signed by `hsm-gateway` (`HSM_GATEWAY_URL`, mTLS with this service's own leaf as the client certificate; the gateway
must carry the name `hsm-gateway`) and the `hot_wallet` seed is never read. Payouts, Fee Bumps and Nonce Fillers all go
through the gateway as a Sign Intent with their operation class (`payout`, `payout`, `cancel`), under a 2 s deadline. The
Hot address is read from `GET /v1/hot/key` at start and pinned; every returned signature is recovered and must equal it
before it is stored. Failures: the gateway's `403 policy_refused` becomes `422 {"error":"policy_refused","rule":...}`;
any gateway/HSM failure, an open circuit breaker (3 failures, open 5 s, checked before the DB lock and before any RPC) or a
signature that does not recover to the Hot address becomes `503 {"error":"hsm_unavailable"}`, and no nonce is consumed.
There is no software fallback: an unreachable gateway at start stops the service.

**No user seed, no Sweep signing (M3b).** wallet-service holds **no** `user_derivation` seed and has **no** `POST /v1/sign/user-address`: Sweeps are
signed by `services/sweep-signer`, the only place that seed exists in plaintext (`sweeper` calls it). Deposit addresses come from the attested xpub
only. The old software master key is needed here only by the LEGACY software Hot Wallet of the e2e regression baseline (`SIGNER_BACKEND=software`, a `!prod`
build, for the `hot_wallet` seed); with `SIGNER_BACKEND=pkcs11` the service has no key at all, and the user seed is in the HSM-sealed vault (`key-admin init`). `cd services/wallet-service && go build -tags prod ./cmd/service`
compiles: the software signer, the sealer and every seed import are behind `!prod` (a test pins that the prod binary links none of them). The
seed ceremonies moved to `key-admin` (`init-seed user_derivation`, `convert-seed`, `migrate-xpub`); `service -init-seed` now only creates the dev
`hot_wallet` seed.

Hot nonce state is per `(chain_id, hot_address)` (migration 000005): a new Hot address starts at its own on-chain nonce.
Rows from before the migration have `hot_address = ''`; only the software backend attributes them to its own address at
start (`storage.AdoptLegacyHotAddress`). The pkcs11 backend never does, so the old rows stay with the old address
until the cutover. A Fee Bump or Nonce Filler for a key signed by another Hot address is refused (`409`).

**Hot cutover (M2c).** `cmd/hot-cutover` (`!prod` build tag, image stage `cutover`, never in this service's image) moves an
existing software Hot to the HSM Hot: `preflight | freeze | settle | drain | verify | complete -switched | abort`, run through
`make hot-cutover-<step>` (`e2e/Makefile`, `dev/Makefile`); runbook `docs/runbooks/hot-cutover.md`. `freeze` writes a `hot_cutover`
row (migration 000006): while it is not `done`, new Hot signs answer `503 hot_cutover` for the whole chain (replays, Fee Bump, Void,
Cancel still work); nothing over HTTP lifts it. `drain` signs the old address's ERC-20 and then native balance to the new Hot with
the old key as ordinary `hot_wallet_signatures` rows (key `hot-cutover:...`, caller `hot-cutover`), so the old address's nonce
sequence stays dense, and posts each tx's gas to ledger-service as CN `hot-cutover`.

`go test -tags gatewayit ./internal/signing` runs the signer against a real `hsm-gateway` container with real SoftHSMv2
(Docker; set `HSM_GATEWAY_IMAGE` to skip the image build) and go-ethereum's simulated EVM.

## What's deliberately not here yet

No Kafka: this service never publishes or consumes events. Withdrawal orchestration belongs to
`withdrawal-service`, which is not built yet (see the phased roadmap in `docs/ARCHITECTURE.md`).
The deposit-address endpoints are still on the plain `:8080` listener; moving them behind mTLS
(`CN=api-gateway`) is deferred (`service-auth-mtls.md` Open Question #4).

## Tests

```bash
make test
```

## Metrics

`GET /metrics` on its own internal port (`METRICS_PORT`, default 9100; never on an API listener, and a test checks that). Go runtime and process
metrics, and the database pool's saturation (`digiwal_db_pool_*`, see `pkg/metrics`).
Both API listeners are also measured: `digiwal_http_request_duration_seconds{service="wallet-service",route,code}`, the route being the
ServeMux pattern (never a path or an idempotency key). Nothing key-related is exported. The image builds from the repo root (it needs `pkg/metrics`).

Operational numbers only, from this service's own database: `digiwal_hot_wallet_frozen{chain_id}` (1 while a Nonce Gap freezes signing; this
service owns the freeze, so it is the authoritative signal), `digiwal_hot_wallet_frozen_from_nonce{chain_id}` (absent when not frozen) and
`digiwal_hot_wallet_next_nonce{chain_id}`. An unreadable database omits them and drops `digiwal_collector_up`: it never reads as "not frozen".


## Deposit addresses from an attested xpub (M3a)

With `XPUB_ATTEST_PUBKEY_FILE` set, new deposit addresses are derived by public derivation (btcd `hdkeychain`, pinned) from the account xpub
`m/44'/60'/0'` in `wallet_xpubs`, at the same `.../0/{index}` path as before, so every existing address is reproduced. The attestation
(signed by the HSM attestation key, verified on every issuance against the key in that file, which lives outside the database) must verify
or issuance latches off with an alert (`503 issuance_unavailable`); stored addresses keep being served. Issuance never reads `wallet_seeds`
(the index counter is `wallet_xpubs.next_index`) and needs no key.
EVM only: Solana's ed25519 (SLIP-10) has no public child derivation. The xpub is created and attested by `key-admin init` (`docs/runbooks/dev-hsm-bringup.md`).
Unset = no new address can be issued (503); since M3b there is no seed-based fallback in the service.
