# chain-watcher

Deposit Detection (`docs/design/deposit-flow.md`): notices transfers to addresses `wallet-service`
issued, waits for Confirmation Depth, and publishes `deposits.detected` / `deposits.confirmed`
(`pkg/events`, `integration-contracts.md` §1). It **only reads the chain**: it never signs or
broadcasts (that is `sweeper`).

## How it works

One `Step` per poll:

1. **Reconcile.** If the node no longer has the blocks we last scanned (a reorg inside the
   unconfirmed window), rewind the watermark to the fork point and drop pending transfers found on
   the abandoned branch. A reorg below an already-*confirmed* transfer stops the service with an
   alert instead (`deposit-flow.md` Open Question #4: out of scope, a human decides).
2. **Scan** new blocks in batches: native transfers (only successful transactions) and `Transfer`
   logs of **registered** ERC-20 contracts (exactly the ERC-20 shape: 3 topics, 32 data bytes).
   Each destination goes through **Address Resolution**: Redis `HGET wallet:address_owners`, then
   `GET /v1/addresses/{address}` on `wallet-service` over mTLS (`CN=chain-watcher`). Only a clean
   `404` means "not ours"; any other failure aborts the range and it is retried, so a transient
   outage can never drop a deposit (invariant #1). Transfers are recorded as `pending` and the
   watermark advances **in the same transaction** (invariant #4).
   **Hot Wallet Outflow is skipped** (invariant #17): a transfer *sent from* the Hot Wallet — the
   sweeper's Gas Top-Up into a deposit address — is not a deposit, so no row is written and nothing is
   published. It is checked before Address Resolution (native: `tx.from`; ERC-20: the log's `from`
   topic) and INFO-logged. The Hot Wallet address comes from `GET /v1/wallet/hot-address` on
   `wallet-service` (mTLS), asked **once at startup** with retries — never an env var — and the
   watcher refuses to scan until it has it. Restart chain-watcher if the Hot Wallet seed is ever
   re-initialised. Deposits mis-credited by Gas Top-Ups *before* this fix are not corrected: an environment that ran the sweeper earlier is reset (ledger + chain-watcher databases and Kafka state together), not repaired (`deposit-flow.md` Open Question #14).
3. **Confirm.** A pending transfer `ConfirmationDepth` blocks deep, whose transaction is still in the
   block we saw it in (receipt check), becomes `confirmed` via `UPDATE ... WHERE status='pending'`.
4. **Publish.** Every recorded-but-unacknowledged event, `detected` before `confirmed`. A row is
   marked published only after Kafka acknowledged it.

Postgres holds the watermark, the hashes of the last 256 scanned blocks, and `observed_transfers`
(natural key `(chain_id, tx_hash, log_index)`, `-1` for native coin). Amounts are `NUMERIC(78,0)`.
A session advisory lock keeps a second instance idle.

## Configuration

| Env | Default | |
|---|---|---|
| `POSTGRES_DSN`, `RPC_URL`, `KAFKA_BROKERS`, `WALLET_SERVICE_URL`, `ASSETS_FILE`, `TLS_CERT_FILE`, `TLS_KEY_FILE`, `TLS_CA_FILE` | required | |
| `REDIS_ADDR` | `localhost:6379` | |
| `CONFIRMATION_DEPTH` | `32` | Sepolia's decided value; use 1-2 on Anvil |
| `START_BLOCK` | current head | where a **brand-new** database starts. Set it to the block of the first issued address, or earlier deposits are never seen |
| `POLL_INTERVAL` | `6s` | |
| `BATCH_BLOCKS` | `100` | blocks per scan range |
| `NEGATIVE_CACHE_TTL` | `30s` | see below |

## Running it

```bash
make certs
make up      # Postgres (own database "chain_watcher") + chain-watcher, http://localhost:8082/healthz
             # needs wallet-service, Redis, Kafka, Anvil: `docker compose` starts what it depends on
make test    # Postgres via testcontainers/TEST_POSTGRES_DSN; Redis via TEST_REDIS_ADDR
```

## Decisions and gaps to know about

- **At-least-once, not exactly-once, on the wire.** The state transition happens once (invariant #3), but publishing is
  driven from the database, so a crash between Kafka's ack and marking the row republishes the same
  event with the same key. The alternative, publishing straight after the `UPDATE`, loses a deposit
  on a crash. `ledger-service` (idempotent on the natural key) and `sweeper` (fresh balance read)
  are built for redelivery.
- **`deposits.detected` can repeat** when a reorg re-includes a transfer on the new branch. It is
  informational; `history-service` should upsert on the key.
- **Negative cache (not in the design).** A busy chain has hundreds of transfers per block to
  addresses that are not ours, each a Redis miss and so an HTTP call. Definite `404`s are cached in
  memory for `NEGATIVE_CACHE_TTL`. Safe because an address only becomes ours when wallet-service
  issues it, before anyone can pay it; errors are never cached.
- **Only top-level transfers.** A transfer made by a contract call (internal transaction, e.g. from
  a smart-contract wallet) is invisible without tracing. The design does not cover it.
- Addresses issued before the Redis hash existed are not in it; they resolve through the endpoint.
- Each service's `docker-compose.yml` is its own Compose project, so `wallet-service` is not on this one's network by that name. For the whole stack on one network use `e2e/` (`make -C e2e up`); on its own, override `WALLET_SERVICE_URL` (e.g. `https://host.docker.internal:8443`).
- Runs against Docker, Kafka and Anvil in `e2e/` (task 2.6: native and ERC-20 deposits end to end, and the Hot Wallet Outflow regression). Not run against Sepolia. Unit-level, it was run against: a real
  EVM (go-ethereum's simulated backend: native transfer, a real ERC-20 `Transfer` log, sender
  recovery, receipts), a scriptable chain for reorgs and failures, and real Postgres and Redis. The resolver was also run once (throwaway
  check, not committed) against the real `wallet-service` binary over mTLS: Redis hit, endpoint fallback, unknown address `404` = not ours,
  `CN=sweeper` refused with `403` (surfaced as an error, not "not ours").

## Metrics

`GET /metrics` on its own internal port (`METRICS_PORT`, default 9100; never on an API listener, and a test checks that). Go runtime and process
metrics, and the database pool's saturation (`digiwal_db_pool_*`, see `pkg/metrics`).
Lag: `digiwal_chainwatcher_watermark_block`, `digiwal_chain_head_block` + `digiwal_chain_head_read_timestamp_seconds`, `digiwal_chainwatcher_confirmation_depth`
(all `{chain_id}`). The head is read by **its own 5s ticker**, not by the poll loop: if the poll loop hangs the watermark freezes while the head
keeps moving, so the lag grows instead of both standing still and reading as zero. A failed head read keeps the last value and does not advance
its read time (staleness shows); a head never read is absent, not 0. It is the head of the RPC node chain-watcher uses, so it cannot show a node
that is itself behind the network.
