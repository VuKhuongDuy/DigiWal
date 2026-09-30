# history-service

The transaction-history **read model** (`docs/ARCHITECTURE.md`): one denormalized MongoDB document
per deposit or withdrawal, built from the four topics it consumes (`integration-contracts.md` §1,
Open Questions #3): `deposits.detected`, `deposits.confirmed`, `withdrawals.broadcast`,
`withdrawals.confirmed`. It never touches a balance and holds nothing that cannot be rebuilt by
replaying the topics (a new consumer group starts from the beginning).

## What works

- One document per deposit (`_id = deposit:<chain_id>:<tx_hash>:<log_index>`) or withdrawal
  (`_id = withdrawal:<idempotency_key>`), with `user_id`, `kind`, `asset_id`, `amount` (decimal
  string, never a float or int64), counterparty `address`, `tx_hash`, `status`, timestamps.
- Statuses only move forward: `pending`/`broadcast` -> `confirmed`. A late or duplicate
  detected/broadcast event never turns a confirmed entry back (`$setOnInsert` upsert for progress,
  `$set` upsert for confirmed), so events may arrive out of order or twice.
- At-least-once: the offset is committed only after the write succeeded. A store outage is retried up to
  `MAX_ATTEMPTS` (10) times, then an `ALERT` log line and the record is skipped (`integration-contracts.md` §5: nothing depends
  on this read model). An event that can never succeed (malformed, unknown schema version) goes to
  `failed_events` and is acknowledged at once.

## Not built / to know

- **No query API.** `GET /v1/history` is a documented gap (`integration-contracts.md` §2): the
  read model's shape here is a dev proposal, not a contract, and nothing may rely on it yet. There
  is an index on `(user_id, updated_at desc)` for the eventual feed.
- `deposits.confirmed` carries no timestamp, so a confirmed deposit's time is when this service
  processed it. A reorg-driven re-detection of a still-pending deposit does not update its block number.
- **The MongoDB code was not run against a MongoDB** (none in the sandbox, no Docker): the consumer
  logic is tested against a fake store, and `mongo_test.go` (pending -> confirmed, out-of-order,
  redelivery) is written but skipped without `TEST_MONGO_URI`. Please run it once
  (`make test` with the env var set) before relying on the upsert semantics.
