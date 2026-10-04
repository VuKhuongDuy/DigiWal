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
Not yet done, with the reason:
- the Prometheus `job_runs` collector and `pkg/jobrun` — they sit on `pkg/metrics` (`monitoring-dashboard.md`
  task 1), which does not exist yet; `store.LastRun` / `store.LastSuccess` are the reads it will use.
- read-only roles on the other stores and the SELECT-only / key-table contract tests (A2).

## Configuration

| Env | |
|---|---|
| `AUDITOR_ENV` | must be `dev` |
| `AUDITOR_ALLOWED_CHAIN_IDS` | comma-separated dev chain ids, default `31337` |
| `POSTGRES_DSN` | the auditor's own database |
| `RPC_URL` | EVM RPC |
| `PORT` | `/healthz`, default 8080 |

`make up` (needs the `auditor` database; the target creates it), `make test`.
