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

- **At-least-once, not exactly-once, on the wire.** The design says a transfer produces at most one
  `deposits.confirmed`. The state transition still happens once (invariant #3), but publishing is
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
- Each service's `docker-compose.yml` is its own Compose project, so `wallet-service` is not on this one's network by that name; override `WALLET_SERVICE_URL` (e.g. `https://host.docker.internal:8443`) until the e2e task (2.6) provides one shared compose.
- Not run against Docker/Kafka/Sepolia/Anvil here (no Docker in the sandbox). What was run: a real
  EVM (go-ethereum's simulated backend: native transfer, a real ERC-20 `Transfer` log, sender
  recovery, receipts), a scriptable chain for reorgs and failures, and real Postgres and Redis. The resolver was also run once (throwaway
  check, not committed) against the real `wallet-service` binary over mTLS: Redis hit, endpoint fallback, unknown address `404` = not ours,
  `CN=sweeper` refused with `403` (surfaced as an error, not "not ours").
