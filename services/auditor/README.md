# auditor

**Dev-environment tool.** Re-checks, after the fact, what the money services decided at write time:
per-transaction rules for deposits and withdrawals, ledger integrity, Hot Wallet signing and an aggregate
on-chain-vs-ledger reconciliation (`docs/design/reconciliation-audit.md`). It reads every store directly and
writes only to its own database (`auditor`), raising findings, never repairing.

It is **not** the production design. It refuses to start unless `AUDITOR_ENV=dev` *and* the RPC's chain id is on
`AUDITOR_ALLOWED_CHAIN_IDS` (default `31337`, local Anvil).

## Status

Skeleton (task A1): the dev-only guard, the `auditor` schema, and a scheduler that records every Audit Run in
`job_runs` (`ok`, `warn`, `fail`, or `error` — a job that could not complete is never `ok`; a panic is `error`).
No rule sets are registered yet: W (A3), D (A4), L (A5), T (A6) and C (A7) each add a `job.Job`.
Read access (task A2) is in place, see below. Not yet done, with the reason:
- the Prometheus `job_runs` collector and `pkg/jobrun` — they sit on `pkg/metrics` (`monitoring-dashboard.md`
  task 1), which does not exist yet; `store.LastRun` / `store.LastSuccess` are the reads it will use.


## Read access (A2)

All SQL against another service's database lives in `internal/source` (SQL anywhere else fails
`TestSQLIsIsolatedAndReadOnly`; the auditor's own database is `internal/store`).

- **Two barriers, both read-only.** The login `auditor_ro` (`roles/`) has `SELECT` only, and column-level where a
  table also holds something the auditor must not read: no `wallet_seeds`, `master_wallet`, `signed_raw_tx`, raw
  txs, outbox payloads or API key hashes. On top, `source.PG` opens sessions with
  `default_transaction_read_only=on` and `source.CheckSelect` rejects anything but a single `SELECT`/`WITH ... SELECT`
  before it is sent. MongoDB: a user with the `read` role (`roles/mongo.js`), and `source.Mongo` only exposes find.
- **`source.Contract`** lists every table and column the auditor reads. `TestStoreContractAndRoles` applies each
  service's real migrations to a fresh database and checks (1) all of it exists, (2) `auditor_ro` can read exactly that
  and nothing else, (3) it cannot write. A rule that needs another column adds it to `Contract` and to the store's roles
  file in the same change. Each service's `make auditor-contract` runs it (there is no CI in the repo yet).
- `make roles` applies the grants to the dev databases. Mongo needs `TEST_MONGO_URI` for `TestMongoReadOnlyUser`.

## Configuration

| Env | |
|---|---|
| `AUDITOR_ENV` | must be `dev` |
| `AUDITOR_ALLOWED_CHAIN_IDS` | comma-separated dev chain ids, default `31337` |
| `POSTGRES_DSN` | the auditor's own database |
| `RPC_URL` | EVM RPC |
| `PORT` | `/healthz`, default 8080 |

`make up` (needs the `auditor` database; the target creates it), `make test`.
