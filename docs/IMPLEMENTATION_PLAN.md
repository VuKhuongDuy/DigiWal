# Implementation plan

This translates the approved design docs (`docs/design/*.md`) into a
concrete build order: modules, tasks, inputs/outputs, and a rough
timeline. It does not make new architecture decisions — every task
here cites the design-doc invariant/section it implements. If a task
would require a decision not already in the docs, it's flagged as
**blocked** instead of guessed.

How to read this: each milestone is a demoable increment (roadmap step
2 in `docs/ARCHITECTURE.md`: "feature-complete locally"). Each task
inside a milestone is sized to be one dev PR. Tasks are ordered by
dependency, not by service — a milestone typically touches 3-4
services because that's what a real user-visible flow requires.

## Decisions to make before/while starting

(Three items now, not two — a migration-tool choice was added after a
Tech Lead review of the actual `wallet-service` code found there wasn't
one yet. Renamed from "Two things to decide" for that reason; nothing
else about how this section works changed.)

1. **Shared event-schema package.** Every service that produces or
   consumes a Kafka event (`chain-watcher`, `ledger-service`,
   `sweeper`, `withdrawal-service`, `history-service`) needs the exact
   same payload shape for `deposits.detected`, `deposits.confirmed`,
   `withdrawals.broadcast`, `withdrawals.confirmed`. Recommending a
   small shared Go module (`pkg/events`, imported via a `go.mod
   replace` directive from each service — same monorepo, no separate
   publish step) holding just the event structs, not business logic.
   Alternative is duplicating the struct in each service and relying
   on code review to catch drift — rejected, that's exactly the kind
   of bug this project is supposed to be practicing catching with
   tooling, not review. Proceeding with the shared-package approach
   unless you object (Task 0.1 below).
2. **Internal Transfer has no design doc.** `docs/ARCHITECTURE.md`
   lists it as core scope (`ledger-service`'s row, the `api-gateway`
   row, the `transfers.internal` topic) but none of the five
   `docs/design/*.md` notes actually design it — no aggregate, no
   invariants, no state machine. It's real scope (touches
   `ledger-service` + `api-gateway`, introduces its own aggregate), so
   it needs a `solution-architect` design note before any task for it
   can be scheduled. Not included in the milestones below — say the
   word and I'll kick off that design note in parallel with M0-M3.
3. **Migration tool: recommending `golang-migrate`.** **New, flagged
   decision** — a gap a Tech Lead review of the actual code surfaced.
   `services/wallet-service/internal/storage/postgres.go` currently
   applies its schema via an inline `CREATE TABLE IF NOT EXISTS` string
   run at connection time (`storage.Connect`) — there is no migration
   tool in this repo at all yet, and no versioned migration files.
   That's fine for a schema that only ever grows new tables, but task
   0.2 below needs to *transform* an existing table's shape (singleton
   `master_wallet` → multi-row `wallet_seeds`) and carry existing data
   forward, which an inline `CREATE TABLE IF NOT EXISTS` string
   structurally cannot do — it has no concept of "already applied" vs.
   "not yet applied," so it can't express "add this column, backfill
   it, then drop that other column" without hand-rolled guards. That
   needs a real migration tool with an applied-versions table and
   ordered up/down `.sql` files.
   - **Recommendation: [`golang-migrate/migrate`](https://github.com/golang-migrate/migrate).**
     Plain, hand-written up/down `.sql` files (no Go-code migrations,
     no ORM), a single `schema_migrations` tracking table, a CLI for
     local/manual use and a Go library (`database/postgres` driver) for
     running migrations from `main()` on service startup, no cgo. This
     fits the project's established pattern of picking the smaller,
     more direct tool over the more "batteries-included" one for the
     same reason `kafka-go` was picked over `confluent-kafka-go` (no
     cgo) and JSON was picked over Avro/Protobuf for Kafka payloads
     (no schema-registry infra to run) — this is currently pure-SQL
     schema work with no need for a Go-code-based migration framework's
     extra machinery (e.g. `goose`'s Go-function migrations, `atlas`'s
     declarative-diffing engine). `golang-migrate` is also the most
     widely used option in the Go ecosystem for exactly this shape of
     problem, so it's a safe default to standardize on across every
     service, not just `wallet-service`.
   - **Convention (applies to every service from here on):** migration
     files live under `services/<service-name>/migrations/`, named
     `{version}_{description}.up.sql` / `{version}_{description}.down.sql`
     with a 6-digit zero-padded sequential version (`golang-migrate`'s
     own default convention, e.g. `000001_baseline_schema.up.sql`). Each
     service's `main()` runs pending migrations against its own
     Postgres connection on startup before serving traffic (same
     "migrate then serve" pattern most Go services using this tool
     follow) — no separate manual migration step to remember, but the
     tracking table means it only ever applies what hasn't already run.
   - **Alternative considered:** `goose` (Go-function or SQL
     migrations, more built-in conveniences like embedding via
     `embed.FS` helpers) — rejected only because `golang-migrate`'s CLI
     + library split and plain-SQL-only file format is slightly more
     minimal for what's currently pure-SQL work, not because `goose` is
     unreasonable; either would work. Flagging this as the real
     decision it is rather than assuming it silently — **your call if
     you'd rather standardize on `goose` instead.**

## Testing strategy

Also new — nothing in this doc previously said how any task actually
gets tested beyond "estimated N PRs." Two things to call out explicitly
so they don't get skipped when a task's row is otherwise busy describing
the feature logic itself:

- **Integration tests against a real Postgres, not mocks, for anything
  that depends on the database's own atomicity/constraint guarantees.**
  `wallet-service`'s derivation-index allocation (`allocateIndex`'s
  atomic `UPDATE ... RETURNING`, and its Milestone-0 sibling, Hot
  Wallet nonce allocation) and `ledger-service`'s sum-to-zero
  enforcement + idempotent replay (`apply()`, task 1.3) are exactly the
  kind of correctness invariant a mocked DB can assert "was called
  correctly" for but can't actually verify — a mock can't reproduce a
  real unique-constraint conflict under concurrent inserts, or confirm
  an `UPDATE ... RETURNING` really is atomic across two goroutines
  racing it. These need to run against a real Postgres instance.
  Required starting at task 0.2 (`wallet-service`) and throughout
  Milestone 1 (`ledger-service`), and expected to be the default for
  any later task making the same kind of claim (`withdrawal-service`'s
  state machine, Milestone 3).
  - **Recommendation: [`testcontainers-go`](https://golang.testcontainers.org/)**
    (Postgres module) to spin up a real, ephemeral Postgres in Docker
    per test run, started and torn down directly from `go test` (e.g.
    a package's `TestMain`) — not a shared, long-lived dev database
    that tests would otherwise have to carefully clean up after
    themselves to avoid cross-test pollution. **Flagging this as a
    tooling decision**, same as the migration-tool one above: the
    project doesn't have a test-infra convention yet either.
    Alternative considered: reusing the existing
    `infra/docker-compose.yml` Postgres service and pointing
    integration tests at it directly — rejected as the default,
    because it means tests can only run one at a time against shared
    mutable state (no test isolation without hand-rolled schema/table
    prefixing), and it couples "can I run this test" to "do I remember
    to have `docker compose up postgres` running first," which
    `testcontainers-go` avoids by managing the container's lifecycle
    from the test binary itself. `docker-compose.yml`'s Postgres stays
    exactly what it already is — the shared stack for local
    service-to-service dev/demo runs, not a test fixture.
  - Each service's migration files (see the decision above) are also
    what seeds schema into these ephemeral test containers — the same
    `.sql` files run in test, dev, and (eventually) production, so
    there's only ever one definition of the schema to keep in sync.
- **A signature-recovery test for the Hot Wallet signing endpoint,
  specifically** (called out on task 0.3 below, since that's where
  `POST /v1/sign/hot-wallet` lands): sign a known payload through the
  real endpoint, then independently recover the signer's public
  key/address from the returned signature — Ethereum's `ecrecover`
  equivalent, `go-ethereum`'s `crypto.SigToPub` /
  `crypto.PubkeyToAddress` — and assert the recovered address equals
  the actual configured Hot Wallet address (not merely "the call
  returned 200 and a hex string that looks like a signature"). This is
  the one test that actually proves the endpoint signed with the Hot
  Wallet's key specifically, as opposed to, say, the wrong derivation
  index or the wrong seed entirely (signing with the user-derivation
  seed by accident, for instance) — a class of bug a happy-path
  "response shape is correct" test would not catch, and one that would
  otherwise only surface once real funds are on the line.

## Module map

| Service | Status | Design doc(s) |
|---|---|---|
| `wallet-service` | **M0 done** (tasks 0.2-0.6): split Hot Wallet / user seeds; `POST /v1/sign/hot-wallet` (atomic nonce, idempotency key, Nonce-Gap flag honoured) and `POST /v1/sign/user-address` (destination pinned to the Hot Wallet); mTLS listener with per-endpoint CN allow-lists; `GET /v1/addresses/{address}`, `GET /v1/wallet/hot-address` (incl. `CN=chain-watcher`, task 2.7); Redis `address -> user_id` hash. Not yet: `withdrawal-service`'s address-lookup grant (task 3.10); deposit-address endpoints are still plain HTTP | `wallet-tiering.md`, `withdrawal-flow.md`, `service-auth-mtls.md` |
| `chain-watcher` | **Done (M2: 2.1, 2.7):** polling, Address Resolution, reorg handling, Confirmation Depth, at-least-once `deposits.detected`/`confirmed` with `user_id`; skips Hot Wallet Outflows (invariant #17). Run in the e2e stack | `deposit-flow.md` |
| `ledger-service` | **Done (M1: 1.1-1.8; M2: 2.2):** double-entry schema, Asset Registry, corrections via `POST /v1/ledger/transactions`, Lock/Settle/Release, balances endpoint (Total/Locked/Available), Screening List, live `deposits.confirmed` consumer; mTLS + CN authz | `ledger.md`, `compliance-screening.md`, `service-auth-mtls.md` (`apply()`/`lock`/`settle`/`release` auth, all PROPOSED) |
| `sweeper` | **Done (M2: 2.3):** consumes `deposits.confirmed`, screening, Gas Top-Up, native and ERC-20 Sweep to the Hot Wallet, bounded retry then `ALERT`. Run in the e2e stack | `deposit-flow.md` |
| `withdrawal-service` | Not started (M3; no code in the repo) | `withdrawal-flow.md`, `wallet-tiering.md`, `compliance-screening.md` |
| `history-service` | **Consumer done (M2: 2.4):** MongoDB read model from the four topics. No query API (documented gap); its Mongo test is skipped without `TEST_MONGO_URI` | `ARCHITECTURE.md` only (read-model, no dedicated design doc — schema-less enough not to need one) |
| `api-gateway` | **Done for M0/M2 (0.7, 2.5):** `POST /users`, API-key auth middleware, `GET /v1/deposit-address`, `GET /v1/balances`. No withdrawal or whitelist routes yet (3.7) | `docs/design/identity-auth.md` (found completely absent from this plan on Tech Lead review — a fully-designed doc with zero tasks against it; added as task 0.7 below) |
| local signing CLI | Not started (M4; no code in the repo; operator tooling, not a service) | `wallet-tiering.md` |

## Milestone 0 — Foundations (no user-visible feature yet)

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 0.1 | `pkg/events` | This doc's recommendation above | Shared Go structs for the 4 Kafka event payloads, versioned | — |
| 0.2 | `wallet-service` | `wallet-tiering.md` invariant #1 | Hot seed split from user-derivation seed — ≥2 sealed seeds, config surface for which key material backs which. **Concrete schema change, migrated with `golang-migrate`** (see "Decisions to make" #3 above): introduces `services/wallet-service/migrations/` with a baseline migration (`000001_baseline_schema`) that reproduces today's inline schema as-is (`master_wallet` singleton + `wallet_addresses`, `IF NOT EXISTS` so it's a no-op against an already-provisioned dev DB and just backfills `golang-migrate`'s `schema_migrations` tracking table), followed by `000002_wallet_seeds` which does the actual transform: <br/>**Before:** `master_wallet(id INT PK CHECK (id=1), sealed_seed BYTEA, next_index INT, created_at)` — one singleton row.<br/>**After:** `wallet_seeds(seed_role TEXT PK CHECK (seed_role IN ('user_derivation','hot_wallet')), sealed_seed BYTEA NOT NULL, next_index INT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now())` — `next_index` keeps its current meaning (the atomic per-user derivation-index counter, `allocateIndex`'s `UPDATE ... RETURNING`) for the `user_derivation` row; the `hot_wallet` row doesn't use per-user derivation at all (it's one address, not one per user) so it carries `next_index = 0`, unused. The `000002` migration's `up` does `INSERT INTO wallet_seeds (seed_role, sealed_seed, next_index, created_at) SELECT 'user_derivation', sealed_seed, next_index, created_at FROM master_wallet WHERE id = 1` (a no-op insert if `master_wallet` was never initialized) then `DROP TABLE master_wallet` — so an already-initialized seed becomes the `user_derivation` row instead of being lost or requiring a fresh `init-master-wallet` run that would orphan already-derived users' addresses (their `derivation_index` values are meaningless against a new, different seed). The `hot_wallet` row is **not** fabricated by this migration — that would mean the migration inventing key material, which is an operator action, not a schema change — it's added afterward by an operator running the (now role-parameterized) init action a second time, e.g. `InitSeed('hot_wallet', sealedSeed)` replacing today's single-purpose `InitMasterWallet`. `down` reverses this: recreate `master_wallet`, copy the `user_derivation` row back into it, drop `wallet_seeds` (the `hot_wallet` row, if present, has nowhere to go back to in the old schema and is dropped — acceptable for a `down` script, which is a rollback path, not a data-preservation guarantee). **Included cleanup, same task:** `services/wallet-service/go.mod`'s `go 1.25.0` directive is bumped to `go 1.26.0` to match `services/wallet-service/Dockerfile`'s `FROM golang:1.26-alpine` — not a build break today (a newer toolchain builds an older `go` directive fine) but real drift worth closing before this Dockerfile/go.mod pattern gets copy-pasted into the next few services. **Testing:** integration tests against a real Postgres (`testcontainers-go`, see "Testing strategy" above) covering (a) the `000002` migration itself — seed an ephemeral container with the pre-migration `master_wallet` row, run the migration, assert it lands correctly in `wallet_seeds.user_derivation` — and (b) concurrent `allocateIndex` calls never allocate the same index twice. | — |
| 0.3 | `wallet-service` | `withdrawal-flow.md` invariants #5, #16; `integration-contracts.md` §2 | `POST /v1/sign/hot-wallet {to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}`, over mTLS, Hot-Wallet-only, no caller-chosen key, no caller-supplied gas params (**PROPOSED**, pending sign-off — `integration-contracts.md` Open Questions #11), idempotent per `idempotency_key` (persist `idempotency_key -> nonce, signed_raw_tx`, never sign twice for the same key). **Dependency change, same task:** this is where `wallet-service` starts constructing and signing real transactions, so `github.com/ethereum/go-ethereum` (currently `v1.10.17 // indirect`, pulled in only transitively via `github.com/miguelmota/go-ethereum-hdwallet`) must be promoted to a direct `require` in `go.mod` and bumped — pin to whatever is the current stable release at implementation time, not a version guessed here. **Testing (required, not optional):** the signature-recovery test described in "Testing strategy" above — sign a known payload via this endpoint, recover the address with `crypto.SigToPub`/`crypto.PubkeyToAddress`, assert it equals the configured Hot Wallet address — plus the usual idempotent-replay-returns-identical-response case. | 0.2, 0.6 |
| 0.4 | `wallet-service` | `withdrawal-flow.md` invariants #6, #7, #16 | Hot Wallet nonce allocation table (atomic `UPDATE...RETURNING`, same pattern as existing `allocateIndex`) + Nonce-Gap freeze flag + the idempotency-key persistence layer task 0.3 depends on | 0.3 |
| 0.5 | `wallet-service` | `deposit-flow.md` invariants #6, #7, #12; `integration-contracts.md` §2 | `POST /v1/sign/user-address {user_id, to_address, asset_id, amount, chain_id}` → `{signed_raw_tx}`, over mTLS. Server-enforces `to_address == Hot Wallet`, never trusting the caller. Sources the signing user address's own EVM nonce via a live `eth_getTransactionCount(address, "pending")` RPC read (**PROPOSED**, `deposit-flow.md` invariant #12) rather than a DB counter. No idempotency key needed (idempotent by construction — caller always signs the live balance). | 0.2, 0.6 |
| 0.6 | `infra/pki`, `wallet-service` | `docs/design/service-auth-mtls.md` | One-time CA generation script (`infra/pki/gen-ca.sh`) + per-service leaf-cert script (`infra/pki/gen-leaf.sh`), gitignored output, docker-compose volume-mount wiring for `wallet-service`'s (and later `withdrawal-service`'s/`sweeper`'s/`chain-watcher`'s) certs; `wallet-service` gains an mTLS listener (`tls.RequireAndVerifyClientCert`) separate from its plain `/healthz` listener, plus the CN-authorization middleware enforcing `service-auth-mtls.md`'s matrix (`hot-wallet` → `{withdrawal-service, sweeper}`, `user-address` → `{sweeper}`, `addresses/{address}` → `{chain-watcher}`, `wallet/hot-address` → `{sweeper, withdrawal-service}`) before any handler logic runs. **Includes the new `GET /v1/addresses/{address}` endpoint** (`integration-contracts.md` §2, fixes the `user_id` gap `deposit-flow.md`'s Address Resolution closes) — `wallet_addresses.address` is already `UNIQUE`-indexed, so this is a new query + handler, no migration. **Also includes the new `GET /v1/wallet/hot-address` endpoint** (`integration-contracts.md` §2, fixes the Hot Wallet Address Discovery gap — returns the address `wallet-service` already derived from its `hot_wallet` seed, so `sweeper`/`withdrawal-service` never need a locally-configured copy that can drift) — no new state, just a handler reading the already-loaded Hot Wallet signer's address. **Also changes `wallet-service`'s Redis cache from a set to a hash** (`internal/storage/redis.go`'s `AddDepositAddress`/`IsDepositAddress` → store and return `user_id`, not just membership), populated at `CreateAddress` time | — |
| 0.7 | `api-gateway` | `docs/design/identity-auth.md` | **New — found on Tech Lead review that this fully-designed doc had zero tasks against it anywhere in this plan.** `api-gateway`'s own Postgres schema (`users`, `api_keys`, `docs/ARCHITECTURE.md`'s row updated to match — Open Questions #1, resolved), `POST /users` (mint `user_id` + plaintext API Key, shown once, invariant #1), the `Authorization: Bearer <api_key>` middleware that resolves and injects the authenticated `user_id` on every other route and rejects with `401` otherwise (invariants #2, #3, #6 — live `key_hash` lookup, no blocklist needed). This is a hard dependency for every other `api-gateway` task (2.5, 3.7) — none of their client-facing endpoints are safe to build against a client-supplied `user_id` until this exists | — |

**DoD:** `wallet-service` can sign a Hot-Wallet transaction over mTLS
and hand out monotonic nonces, and can sign a Sweep transaction from a
specified user address to the Hot Wallet only, with both signing
endpoints rejecting any caller whose certificate CN isn't on that
endpoint's allow-list; `api-gateway` can mint a user + API Key and
reject any request without a valid one; nothing calls either signing
endpoint for real yet. Estimated 7 PRs.

## Milestone 1 — Ledger core (`ledger-service`)

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 1.1 | `ledger-service` | `ledger.md` schema section | Postgres migrations, using **`golang-migrate`** (see "Decisions to make" #3 above — same tool and `services/ledger-service/migrations/{version}_{description}.up/down.sql` convention as `wallet-service`'s task 0.2, this being a fresh schema with no prior data to carry forward): `accounts`, `entries`/`ledger_transactions`, `locks`, `asset_registry` (incl. `hot_floor`, `hot_ceiling`, `min_withdrawal_amount` fields). **Every `amount`/`*_balance` column is `NUMERIC(78,0)`, never `bigint`** (`ledger.md` invariant #10 — a `bigint` overflows at ~9.2 ETH in wei) | — |
| 1.2 | `ledger-service` | `ledger.md` Asset Registry | Static config loader (env/YAML), validated at startup | 1.1 |
| 1.3 | `ledger-service` | `ledger.md` invariants #2, #9, #11; `service-auth-mtls.md` invariant #7 (PROPOSED) | `apply(idempotency_key, source_type, entries[])` — atomic, idempotent, sum-to-zero-per-asset enforced in one DB transaction, served behind an mTLS listener with CN-authorization restricting it to `CN=withdrawal-service` only. **Scope: deposits and corrections only** — a withdrawal Reservation no longer goes through this endpoint (see task 1.7). For `source_type='correction'`, every entry must carry `corrects_entry_id`; `ledger-service` computes the entry's actual amount server-side as the exact negation of the referenced original, never trusting a caller-supplied amount (invariant #11 — this is what stops the endpoint minting an arbitrary credit). Debit check (corrections only — deposits never debit) uses Available Balance (`total_balance - locked_balance`), not bare `total_balance` (invariant #9) | 1.1, 0.6 |
| 1.4 | `ledger-service` | `pkg/events` (0.1) | Kafka consumer scaffold for `deposits.confirmed` (interface only — real message comes in M2) | 0.1 |
| 1.5 | `ledger-service` | `compliance-screening.md` invariant #1 | Screening-List check in the deposit-consumption path; `flagged_deposits` table, never auto-applied | 1.3, 1.4 |
| 1.6 | `ledger-service` | invariants #1–#2, #9 | Unit + integration tests: overdraft rejection (Available-Balance-based, not Total), sum-to-zero-per-asset, idempotent replay. **Integration tests run against a real Postgres via `testcontainers-go`** (see "Testing strategy" above) — these invariants specifically depend on real transaction/constraint behavior a mock can't reproduce | 1.3 |
| 1.7 | `ledger-service` | `ledger.md` Lock/Settle/Release diagram and invariants #1, #6–#10; `service-auth-mtls.md` invariant #7 (PROPOSED) | `POST /v1/ledger/locks` (`lock(idempotency_key, user_id, asset_id, amount)` — resolves the Account server-side, never takes `account_id`), `.../{key}/settle`, `.../{key}/release`. **Settle is one atomic Postgres transaction** — the lock-status guard, both balance updates (debit user, credit chain account), and both entry inserts together, never split across two transactions and never routed through task 1.3's `apply()` as a nested call (an earlier design draft did both, which either let a crash leave a lock `settled` with no Entry, or double-decremented `total_balance` — see `ledger.md`'s Diagram section for the fixed version). Same mTLS + CN-authorization as task 1.3, `CN=withdrawal-service` only, on all three endpoints. **Also includes `GET /v1/ledger/balances/{user_id}`** (`integration-contracts.md` §2, new — fixes a real gap: `api-gateway`'s `GET /v1/balances` had a decided response shape but nothing on the `ledger-service` side to call) — one row per Asset Registry entry (`total_balance`/`locked_balance`/`available_balance`), `0`/`0`/`0` for an asset with no Account row yet rather than a missing entry, `CN=api-gateway` only | 1.1, 0.6 |
| 1.8 | `ledger-service` | invariants #6–#8 | Unit + integration tests: Lock idempotent creation, Lock resolves exactly once (Settled xor Released, never both, never twice), Settle's atomicity under a simulated mid-transaction crash, Release posts no Entry. Real Postgres via `testcontainers-go`, same reasoning as task 1.6 | 1.7 |

**DoD:** `ledger-service` correctly applies/rejects deposits and
corrections, locks/settles/releases withdrawal Reservations, and flags
blacklisted deposits, fully testable in isolation (no other unbuilt
service needed — publish a fake `deposits.confirmed` message by hand,
or call `lock`/`settle`/`release` directly). Estimated 7-8 PRs.

## Milestone 2 — Deposit flow, end to end

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 2.1 | `chain-watcher` | `deposit-flow.md` | Postgres schema (watermark, `observed_transfers`, now including `user_id` per invariant #1's rewrite, **plus `published_at TIMESTAMPTZ` nullable per invariant #3's revision** — fixes a real gap: publishing `deposits.confirmed` as a side effect of the `pending→confirmed` UPDATE could silently drop the event forever on a crash in between; the poll loop now separately re-queries `status='confirmed' AND published_at IS NULL` and retries, delivering at-least-once instead), Sepolia RPC polling, Address Resolution (Redis `HGET` against `wallet-service`'s address→`user_id` hash, falling back to `GET /v1/addresses/{address}` over mTLS with `chain-watcher`'s own leaf certificate from task 0.6 on a cache miss — never a direct cross-service Postgres read), Confirmation Depth = 32 blocks, publishes `deposits.detected`/`deposits.confirmed` with `user_id` included, exact payload per `integration-contracts.md` §1 | 0.1, 0.6 |
| 2.2 | `ledger-service` | 2.1's real topic | Wire M1.4's consumer scaffold to the live topic. **Never gives up on a failing message** — retries indefinitely rather than skipping (`integration-contracts.md` §5) | 1.5, 2.1 |
| 2.3 | `sweeper` | `deposit-flow.md` invariants (Gas Top-Up, Sweep idempotency #8, #10, #12, #13, #15, #16) | Asset Registry loader (same static-config pattern as task 1.2 — fixes a gap found on Tech Lead review: `sweeper` needs `asset_type`/`contract_address` to branch native-vs-ERC20 for both the balance query and the signing call, `ledger.md`'s Bounded context(s)). Consumes `deposits.confirmed` (now carrying `user_id`, needed for `POST /v1/sign/user-address`'s request — `deposit-flow.md`'s Sweep term), independent compliance check, Gas Top-Up (0.001 ETH) via `wallet-service /v1/sign/hot-wallet` with idempotency key `gas-topup:{chain_id}:{tx_hash}:{log_index}` (**Decided**), fresh-balance-read before signing, sweeps to Hot Wallet via `wallet-service /v1/sign/user-address` — `to_address` sourced from `GET /v1/wallet/hot-address` (invariant #15, **not** a `HOT_WALLET_ADDRESS` env var — that copy can drift from what `wallet-service` actually derived), fetched once at startup and cached. **Failure handling:** bounded retry (10 attempts per message) then `ALERT`-log and skip, offset still advances — never a dead-letter topic (invariant #16, `integration-contracts.md` §5). Calls made with `sweeper`'s own leaf certificate (`CN=sweeper`, from task 0.6's tooling) | 0.3, 0.5, 2.1, 0.6 |
| 2.4 | `history-service` | `deposits.confirmed`/`deposits.detected` | Minimal MongoDB consumer, denormalized read model. **Failure handling:** same as `sweeper` — bounded retry (10 attempts) then `ALERT`-log and skip, offset still advances (`integration-contracts.md` §5) — safe since no invariant depends on this read model | 2.1 |
| 2.5 | `api-gateway` | — | `GET /deposit-address` (proxies `wallet-service`'s `POST /v1/users/{userID}/deposit-address` — **not** `GET`, since `POST` is already idempotent get-or-create and this avoids ever surfacing a `404` to a first-time user, `integration-contracts.md` §2), `GET /balances` (proxies `ledger-service`'s new `GET /v1/ledger/balances/{user_id}`, task 1.7 — returns `total_balance`/`locked_balance`/`available_balance` per asset, `integration-contracts.md` §2, `ledger.md`'s Total/Locked/Available split). Both behind task 0.7's auth middleware — `user_id` comes from the resolved API Key, never a client-supplied field | 1.7, 0.7 |
| 2.6 | all of the above | — | e2e test against a local Sepolia fork (Anvil, now running via `docker compose up anvil` — see `infra/docker-compose.yml` and `docs/DEVELOPMENT.md`): simulate a deposit, assert balance updates | 2.1–2.5 |
| 2.7 | `wallet-service`, `chain-watcher`, `e2e` | `deposit-flow.md` invariant #17 (**Decided** by the Tech Lead, Open Questions #12 resolved; **implemented** — commits 07fcb1d, 68e7dac, `make e2e` green). Remediation of earlier mis-credits is a reset, not a repair (`deposit-flow.md` Open Questions #14, Decided: drop the ledger and `chain-watcher` databases and Kafka state together in any longer-lived environment whose sweeper ran pre-fix) — no script or runbook task | **Fix: Gas Top-Up credited as a deposit (found by 2.6's e2e test). Non-trivial — two services plus an authz grant, hence the design note.** (1) `wallet-service`: add `CNChainWatcher` to `authz.HotAddressReaders` (`internal/authz/authz.go`) + authz test (chain-watcher gets `200` on hot-address, still `403` on both `/v1/sign/*`). (2) `chain-watcher`: at startup, before the first `Step`, call `GET /v1/wallet/hot-address` with the existing mTLS client (`cmd/service/main.go`, mirror `sweeper`'s `fetchHotAddress`: bounded retry, then exit non-zero; no env var); add a `HotWallet common.Address` field to `watcher.Watcher`; in `finder.native` skip when `tx.From == HotWallet` **before** `resolve` and the receipt fetch, in `finder.erc20` skip when the log's `from` topic == `HotWallet` before `resolve`; INFO-log each skip; no migration, no event/payload change. Tests in `internal/watcher`: native and ERC-20 Hot Wallet Outflow to a Watched Address produce no row, no event, no resolver call, and the watermark still advances; the same transfer from another sender is still recorded; re-scan/rewind is idempotent; no scan happens before the address is known. Update `services/chain-watcher/README.md`. (3) `e2e`: remove the `E2E_KNOWN_BUGS` skip from subtest "gas top-up is not credited as a deposit" (`e2e/e2e_test.go`) and the "Known bug" section of `e2e/README.md`; make the subtest wait until the top-up is past Confirmation Depth (mine blocks) before asserting bob's ETH balance is `0`, or it can pass vacuously. e2e compose needs no change (`chain-watcher` already has its leaf cert; no env var added). No `sweeper`/`ledger-service`/`history-service` change | 2.1, 2.3, 2.6 |

**DoD:** a user can request a deposit address, send testnet ETH, and
see their balance update after 32 blocks — first real demo. Estimated
6 PRs.

## Milestone 3 — Withdrawal flow, end to end

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 3.1 | `withdrawal-service` | `withdrawal-flow.md` invariant #11 | Schema (state machine `requested→validated→reserved→signed→broadcast→confirmed`), `POST /withdrawals` idempotent creation (`INSERT...ON CONFLICT DO NOTHING` first, before anything else). **Key scope depends on `withdrawal-flow.md` invariant #20 (PROPOSED, Open Questions #11): `withdrawal_id` UUID PK + `UNIQUE(user_id, idempotency_key)` — get approval before writing the migration.** A duplicate `POST` only reads the row back, never re-validates (invariant #11, Decided option B) | 0.1 |
| 3.2 | `withdrawal-service` | invariant #10, whitelist (below), `compliance-screening.md` | Asset Registry loader (same static-config pattern as task 1.2 — fixes a gap found on Tech Lead review: `withdrawal-service` needs `min_withdrawal_amount` for this task's own check, `ledger.md`'s Bounded context(s)). Validation: min-withdrawal-amount, whitelist-active check, blacklist check, **and the own-deposit-address check (`withdrawal-flow.md` invariant #18, task 3.10)** — all before any ledger reservation | 3.1, 3.6 |
| 3.3 | `withdrawal-service` | invariants #1–#6, #16 | Reserve (via `ledger-service`'s `POST /v1/ledger/locks` — `lock(idempotency_key, user_id, asset_id, amount)`, never `account_id`) → sign (via `wallet-service /v1/sign/hot-wallet`, passing the Withdrawal Request's own `idempotency_key`, using `withdrawal-service`'s own leaf certificate from task 0.6) → broadcast → poll to Withdrawal Confirmation Depth = 64 blocks → **Settle** (`.../{key}/settle`) at the `confirmed` transition, posting the real debit for the first time. Any failure before `confirmed` (signing failure, broadcast failure, abandonment) calls **Release** (`.../{key}/release`) instead — no Entry ever existed to compensate (`withdrawal-flow.md`'s revised invariant #3). **Publishes `withdrawals.broadcast` at the `broadcast` transition and `withdrawals.confirmed` at `confirmed`** (payloads `integration-contracts.md` §1, `pkg/events`), recorded-then-published with a `published_at`-style marker so a crash can't drop one (same at-least-once pattern as `chain-watcher`). Lock/sign keys are the Withdrawal Key `withdrawal:{withdrawal_id}` if invariant #20 is approved (task 3.11) | 3.2, 0.3, 0.4, 0.6, 1.7 |
| 3.4 | `withdrawal-service` | invariants #7–#9 | Nonce Gap detection, `disputed` state, freeze via `wallet-service`, operator-resolution stub (manual DB action, no UI yet). **Freeze interface: `withdrawal-flow.md` invariant #19 (PROPOSED, Open Questions #12 — approve first).** (1) `wallet-service`: `POST /v1/wallet/nonce-gap {chain_id, from_nonce}` on the mTLS listener, new `authz.NonceGapReporters = {CNWithdrawalService}`, handler over the existing `storage.FreezeNonceGap` (returns the effective `frozen_from_nonce`/`frozen_at`; `404` no nonce row; `422` if `from_nonce >= next_nonce`); tests: LEAST semantics, repeat is a no-op, `CN=sweeper`/others `403`, sign endpoint `503` afterwards while an already-signed key still replays. **No unfreeze endpoint** — document the operator SQL in `wallet-service`'s README. (2) `withdrawal-service`: mark requests with nonce ≥ N `disputed`, then call the endpoint, retrying until `200` | 3.3, 0.4 |
| 3.5 | `withdrawal-service` | invariant #9 (revised scope — see `withdrawal-flow.md`'s Compensating Entry definition) | Compensating Entry — **only** reached from `disputed` → abandoned (post-Settle reorg case; the ordinary pre-confirm failure path is task 3.3's Release, not this) — via `ledger-service`'s `POST /v1/ledger/transactions` with `source_type='correction'`, the deterministic `{idempotency_key}:refund` key, and `corrects_entry_id` set on every entry (`ledger.md` invariant #11 — the endpoint recomputes the amount server-side, `withdrawal-service` never gets to assert it) | 3.4 |
| 3.6 | `withdrawal-service` | new Withdrawal Whitelist section | Add (pending → active after 24h cooldown) / remove (instant) whitelist entries; **creation also refuses an own-deposit-address (invariant #18, task 3.10)** | 3.1 |
| 3.7 | `api-gateway` | — | `POST /withdrawals`, `GET /withdrawals/{idempotency_key}` (returns only the caller's own request — `404` otherwise, invariant #20 PROPOSED), `POST /whitelist`, `DELETE /whitelist/:id` — all behind task 0.7's auth middleware, forwarded to `withdrawal-service` over **mTLS as `CN=api-gateway`** (Decided, `service-auth-mtls.md` Open Questions #4, this hop) | 3.2, 3.6, 0.7 |
| 3.8 | all of the above | — | e2e test incl. a forced Nonce Gap (reorg simulation on the local fork, via Anvil's `anvil_snapshot`/`anvil_revert` — `docker compose up anvil`, see `docs/DEVELOPMENT.md`) | 3.1–3.5 |
| 3.9 | `withdrawal-service` | `withdrawal-flow.md` invariant #17 (new — Reaper) | Periodic Reaper loop: mark `requested` rows `failed` past the Stuck Request Timeout (**5 min, PROPOSED value**; no ledger call — Decided option B, invariant #11); auto-Release `validated` (**PROPOSED addition**, Open Questions #14) and `reserved`/`signed` requests past the Stuck Reservation Timeout; auto-Release `broadcast` requests past the Stuck Broadcast Timeout **only when** a live nonce read proves the Hot Wallet's on-chain nonce has already passed this tx's nonce; otherwise alert an operator rather than guess. **Decided (Tech Lead, `withdrawal-flow.md` Open Questions #3): Stuck Reservation Timeout = 5 minutes, Stuck Broadcast Timeout = 128 blocks.** Every transition guarded `WHERE status = <expected>` so the Reaper never races the in-line handler | 3.3, 3.4 |
| 3.10 | `wallet-service`, `withdrawal-service` | `withdrawal-flow.md` invariant #18 (rule, authz grant and `422 destination_is_deposit_address` **Decided** by the Tech Lead — `deposit-flow.md` Open Questions #13 option (b), `withdrawal-flow.md` Open Questions #9; `destination_is_system_wallet` code PROPOSED; Warm source open, Open Questions #10) | **Reject withdrawals (and whitelist entries) to our own deposit addresses, the Hot Wallet and the Warm Wallet.** (0) Hot Wallet address from `GET /v1/wallet/hot-address` once at startup (no withdrawal/whitelist request accepted before it is known); byte-wise comparison against Hot (and Warm, once its source is decided — until then leave a TODO and a skipped test) -> `rejected` + `422 destination_is_system_wallet`, run before the deposit-address lookup. (1) `wallet-service`: add `CNWithdrawalService` to `authz.AddressLookupCallers` (`internal/authz/authz.go`); tests: `withdrawal-service` gets `200`/`404` on `GET /v1/addresses/{address}`, `chain-watcher` unchanged, any other CN still `403`, and `withdrawal-service` still cannot reach `/v1/sign/user-address`. (2) `withdrawal-service`: an address-lookup client using its existing mTLS certificate, live call with **no negative cache**; wire into validation as the last check before Reservation (task 3.2) and into whitelist creation (task 3.6). Outcomes: `200` -> `rejected` + `422 destination_is_deposit_address`; `404` -> continue; timeout/5xx/403/anything else -> fail closed (`503`, request not marked `rejected`, never treated as `404`). Tests: own deposit address rejected at request time and at whitelist creation; a pre-existing whitelisted such address still rejected at request time; wallet-service down -> `503`, no Lock created, the row stays `requested` and a retry with the **same** key only reads it back (invariant #11, option B) while a retry with a **new** key after recovery succeeds; unknown address proceeds; Hot Wallet destination rejected. Add to task 3.8's e2e: a withdrawal to a user's own deposit address is rejected and the balance is untouched. No `chain-watcher` change | 3.2, 3.6, 0.6, 2.7 |
| 3.11 | `withdrawal-service`, `pkg/events`, `history-service` | `withdrawal-flow.md` invariant #20 (**PROPOSED**, Open Questions #11 — do not start until approved) | **Per-user idempotency keys + server-minted Withdrawal Key.** `withdrawal-service`: `withdrawal_id` UUID PK, `UNIQUE(user_id, idempotency_key)`, `GET` scoped to the authenticated `user_id`; derive `withdrawal:{withdrawal_id}` for lock/settle/release and `/v1/sign/hot-wallet`, `withdrawal:{withdrawal_id}:refund` for a Compensating Entry. `pkg/events`: add `withdrawal_id` (uuid) to `WithdrawalBroadcast`/`WithdrawalConfirmed` and use it as `Key()`/`IdempotencyKey()`. `history-service`: key withdrawal documents on `withdrawal:<withdrawal_id>` (no data migration needed — no withdrawal has ever been published). Tests: two users with the same key get two requests; user B's `GET` of A's key is `404`; a client key of `gas-topup:...` never reaches `wallet-service` | 3.1, 3.3 |

**DoD:** a user can withdraw to a whitelisted address and see it
confirm; a simulated reorg correctly freezes and disputes affected
withdrawals; a Withdrawal Request stuck before broadcast is
automatically reclaimed rather than freezing the user's funds forever.
Estimated 9-10 PRs.

## Milestone 4 — Wallet tiering operator tooling

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 4.1 | `withdrawal-service` | `wallet-tiering.md` invariants #4, #6 | Sweep-monitor loop: Hot balance > Ceiling → creates/reuses a Treasury Sweep Attempt row (**PROPOSED** idempotency-key source, `wallet-tiering.md` invariant #6) → automatic Treasury Sweep to Warm via `wallet-service /v1/sign/hot-wallet` | 3.3 |
| 4.2 | local CLI (new, standalone) | `wallet-tiering.md` invariants #2, #3 | Decrypts Warm/Cold sealed seed locally, signs a manual replenishment tx — never touches any always-on service's config | 0.2 |
| 4.3 | `withdrawal-service` | `wallet-tiering.md` Open Q #5 area | Low-Hot-balance alert (log/metric only; full alerting is roadmap step 3) | 4.1 |

Estimated 3 PRs.

## Milestone 5 — Compliance/Screening completion

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 5.1 | `ledger-service`, `sweeper`, `withdrawal-service` | `compliance-screening.md` | Shared Screening List loader (same static-config pattern as Asset Registry) | 1.2 |
| 5.2 | `ledger-service` | `compliance-screening.md` Screening Resolution | Operator resolution of a Flagged Deposit (`approved` replays original idempotency key through `apply()`; `rejected` stays terminal) | 1.5, 5.1 |

(Withdrawal-side blacklist check already shipped in 3.2.) Estimated 2 PRs.

## Rough timeline

Counting PRs rather than calendar days, since throughput here depends
on how many PRs you can review per day, not developer headcount:

| Milestone | PRs | Cumulative |
|---|---|---|
| 0 — Foundations | 7 | 7 |
| 1 — Ledger core | 7-8 | 14-15 |
| 2 — Deposit flow | 6 | 20-21 |
| 3 — Withdrawal flow | 9-10 | 29-31 |
| 4 — Tiering tooling | 3 | 32-34 |
| 5 — Compliance completion | 2 | 34-36 |

(Milestones 0, 1 and 3 all grew from the original estimate, each on
Tech Lead review surfacing real, previously-undesigned-or-unplanned
scope, not padding: `identity-auth.md` had zero tasks anywhere in this
plan despite being fully designed (task 0.7); `ledger.md`'s Lock/Settle/
Release split added tasks 1.7/1.8; `withdrawal-flow.md`'s new Reaper
invariant added task 3.9.) At a sustainable pace of 2-4 reviewed
PRs/day, milestones 0-3 (the "it actually works end-to-end" core) land
in roughly **8-13 working days**; 4-5 add **1-2 more**. Not included:
Internal Transfer (blocked on a design doc — see above) and the
observability phase (roadmap step 3, deliberately deferred by several
open questions already).

## Integration contracts

A companion doc consolidating the exact Kafka payload shapes and REST
endpoints referenced piecemeal across the five design docs (so
producer and consumer code can't drift) is being drafted separately by
the Solution Architect: `docs/design/integration-contracts.md`.
