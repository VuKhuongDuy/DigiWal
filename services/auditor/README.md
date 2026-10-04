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
Registered so far: W (A3), D (A4), L (A5), T (A6). C (A7) adds its job.
Read access (task A2) is in place, see below. Not yet done, with the reason:
- the Prometheus `job_runs` collector and `pkg/jobrun` — they sit on `pkg/metrics` (`monitoring-dashboard.md`
  task 1), which does not exist yet; `store.LastRun` / `store.LastSuccess` are the reads it will use.


## Withdrawal rules (A3)

`withdrawal_audit` applies W1-W10 to every `confirmed`, `failed` or `rejected` request once it has been final for
the Grace Period (default 2 runs = 10 min). `internal/rules/withdrawal` is pure (Facts in, Violations out, one fixture
test per rule); `internal/source/withdrawal.go` reads the facts from the stores; `internal/audit` runs it.

Each run re-evaluates every reference that has a pending or open finding, audits requests that became final since the
cursor, and looks for ledger Settles with no request. A mismatch is remembered in `pending_findings` and becomes a
`suspicious_transactions` row only when the next run sees it again, except W6 and W8 (signature and tx-hash facts
cannot be a timing artefact between stores). W5 is a warn that becomes critical after `AUDIT_ESCALATE` (3 runs); W9 is a
warn. A violation that goes away is `resolved`, never deleted; one that returns reopens. The job's Result counts open
findings: `warn` if only warns, `fail` if any critical is open (so it stays `fail` until fixed or acknowledged).

Needs `LEDGER_RO_DSN`, `WALLET_RO_DSN`, `WITHDRAWAL_RO_DSN`, `HISTORY_RO_MONGO_URI` (the `auditor_ro` logins from
`make roles`); `AUDIT_INTERVAL`, `AUDIT_GRACE`, `AUDIT_ESCALATE` override the defaults.

## Deposit rules (A4)

`deposit_audit` applies D1-D7 to every observed transfer confirmed for the Grace Period. A deposit is identified by
`chain_id:tx_hash:log_index` (log_index -1 = native coin), the key the ledger and history-service already use.
Same machinery as W (pure rules in `internal/rules/deposit`, two-pass, resolve/reopen, D6 warn -> critical).
Two extra scans find what has no starting row: ledger credits with no observed transfer (D2), and **D7**, a chain scan
for ERC-20 `Transfer` logs and plain native transfers to a deposit address that chain-watcher never recorded. D7 only
covers blocks up to chain-watcher's watermark and older than the Grace Period, so a transfer the watcher has not reached
is not "missed". Native transfers made by contracts (internal calls) are not seen: no trace API (same limit as the design).
The D7 cursor is a block number; `AUDIT_SCAN_START_BLOCK` (default 0) is where it starts; the first run on a long-lived
chain walks every block once. Needs `WATCHER_RO_DSN` too.

## Ledger integrity (A5)

`ledger_integrity` runs L1-L9 every run, as aggregates over ledger-service's database in one `REPEATABLE READ, READ ONLY`
transaction (`source.LedgerChecks`): transactions balance (L1), `total_balance` = sum of Entries (L2), `locked_balance` =
sum of active Locks (L3), no negative Available (L4), each asset sums to zero (L5), corrections exactly negate their target
once (L6), settled Lock <=> one `withdrawal_settle` (L7), well-formed Gas Fee keys (L8), the `entries` immutability
triggers exist and are enabled (L9, named in `rules/ledger`). All critical and recorded at once (a snapshot cannot disagree
with itself, so there is no lag to wait out). Each rule reports at most `source.MaxPerRule` (200) offenders per run; if a
rule hits the cap, nothing is resolved that run, since absence proves nothing then. Needs only `LEDGER_RO_DSN`. A full scan
each run is fine at dev size; the design's incremental scan with a nightly full pass is not built.

## Treasury and Hot Wallet signing (A6)

`treasury_audit` scans blocks (older than the Grace Period, `AUDIT_*` confirmations deep) for every transfer with a Hot, Warm
or Cold address on one side (ERC-20 `Transfer` logs of the registered tokens, and plain native transfers), records each in
`treasury_movements` with a kind (`hot_to_warm`, `warm_to_hot`, `sweep`, `gas_topup`, `withdrawal`, `deposit_into_tier`,
`unexplained`, ...), and applies T1-T5, all recorded at once. Warm and Cold are moved by the offline CLI and recorded nowhere
else, so the chain is the only source.

- **T1** the Hot address's chain transaction count is not above `hot_wallet_nonces.next_nonce` (chain read first, database
  second, so a race cannot false-alarm). **T2** every mined Hot tx is one wallet-service signed for its nonce (hash of the
  stored original, each Fee Bump and cancel). **T3** every Hot signature has a known purpose (`withdrawal:` with a request,
  `gas-topup:`, `sweep:`, `treasury:`) and an allowed caller (`rules/treasury.SignerPolicy` mirrors wallet-service's authz).
  **T4** value left Hot in a tx with no signature at all (the movement is then `unexplained`). **T5** a movement out of Warm
  or Cold is `info` (no operator-side record exists), `warn` if it leaves our tiers (Open Question 5: `info` as the doc proposes).
- To hash the original signed tx, the auditor reads `hot_wallet_signatures.signed_raw_tx` (a signed tx is not key material;
  added to the contract and the grants). Nothing else about keys is readable.
- **Addresses come from configuration**, not wallet-service (it calls no service API, and `system-addresses` is
  withdrawal-service's until A8): `AUDIT_HOT_ADDRESS` (required; the address `GET /v1/wallet/hot-address` returns),
  `AUDIT_WARM_ADDRESS` (required; `infra/treasury.yaml`), `AUDIT_COLD_ADDRESS` (optional, not configured anywhere yet).
  A wrong Hot address makes T1 and T2 quiet or noisy, so check it. Needs `LEDGER_RO_DSN`, `WALLET_RO_DSN`, `WITHDRAWAL_RO_DSN`.
- Native transfers made inside contract calls are not seen (no trace API). The scan starts at `AUDIT_SCAN_START_BLOCK`.

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
