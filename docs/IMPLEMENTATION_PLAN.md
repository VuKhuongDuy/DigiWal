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

## Two things to decide before/while starting

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

## Module map

| Service | Status | Design doc(s) |
|---|---|---|
| `wallet-service` | Skeleton exists (HD derive, seal/unseal, Postgres+Redis) — no signing endpoint, no tiering yet | `wallet-tiering.md`, `withdrawal-flow.md` |
| `chain-watcher` | Not started | `deposit-flow.md` |
| `ledger-service` | Not started | `ledger.md`, `compliance-screening.md` |
| `sweeper` | Not started | `deposit-flow.md` |
| `withdrawal-service` | Not started | `withdrawal-flow.md`, `wallet-tiering.md`, `compliance-screening.md` |
| `history-service` | Not started | `ARCHITECTURE.md` only (read-model, no dedicated design doc — schema-less enough not to need one) |
| `api-gateway` | Not started | `ARCHITECTURE.md` only |
| local signing CLI | Not started (operator tooling, not a service) | `wallet-tiering.md` |

## Milestone 0 — Foundations (no user-visible feature yet)

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 0.1 | `pkg/events` | This doc's recommendation above | Shared Go structs for the 4 Kafka event payloads, versioned | — |
| 0.2 | `wallet-service` | `wallet-tiering.md` invariant #1 | Hot seed split from user-derivation seed — ≥2 sealed seeds, config surface for which key material backs which | — |
| 0.3 | `wallet-service` | `withdrawal-flow.md` invariants #5, #16; `integration-contracts.md` §2 | `POST /v1/sign/hot-wallet {to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}`, over mTLS, Hot-Wallet-only, no caller-chosen key, no caller-supplied gas params (**PROPOSED**, pending sign-off — `integration-contracts.md` Open Questions #11), idempotent per `idempotency_key` (persist `idempotency_key -> nonce, signed_raw_tx`, never sign twice for the same key) | 0.2 |
| 0.4 | `wallet-service` | `withdrawal-flow.md` invariants #6, #7, #16 | Hot Wallet nonce allocation table (atomic `UPDATE...RETURNING`, same pattern as existing `allocateIndex`) + Nonce-Gap freeze flag + the idempotency-key persistence layer task 0.3 depends on | 0.3 |
| 0.5 | `wallet-service` | `deposit-flow.md` invariants #6, #7, #12; `integration-contracts.md` §2 | `POST /v1/sign/user-address {user_id, to_address, asset_id, amount, chain_id}` → `{signed_raw_tx}`, over mTLS. Server-enforces `to_address == Hot Wallet`, never trusting the caller. Sources the signing user address's own EVM nonce via a live `eth_getTransactionCount(address, "pending")` RPC read (**PROPOSED**, `deposit-flow.md` invariant #12) rather than a DB counter. No idempotency key needed (idempotent by construction — caller always signs the live balance). | 0.2 |

**DoD:** `wallet-service` can sign a Hot-Wallet transaction over mTLS
and hand out monotonic nonces, and can sign a Sweep transaction from a
specified user address to the Hot Wallet only; nothing calls either
endpoint for real yet. Estimated 5 PRs.

## Milestone 1 — Ledger core (`ledger-service`)

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 1.1 | `ledger-service` | `ledger.md` schema section | Postgres migrations: `accounts`, `entries`/`ledger_transactions`, `asset_registry` (incl. `hot_floor`, `hot_ceiling`, `min_withdrawal_amount` fields) | — |
| 1.2 | `ledger-service` | `ledger.md` Asset Registry | Static config loader (env/YAML), validated at startup | 1.1 |
| 1.3 | `ledger-service` | `ledger.md` invariants #1, #2 | `apply(idempotency_key, entries[])` — atomic, idempotent, sum-to-zero enforced in one DB transaction | 1.1 |
| 1.4 | `ledger-service` | `pkg/events` (0.1) | Kafka consumer scaffold for `deposits.confirmed` (interface only — real message comes in M2) | 0.1 |
| 1.5 | `ledger-service` | `compliance-screening.md` invariant #1 | Screening-List check in the deposit-consumption path; `flagged_deposits` table, never auto-applied | 1.3, 1.4 |
| 1.6 | `ledger-service` | invariants #1–#2 | Unit + integration tests: overdraft rejection, sum-to-zero, idempotent replay | 1.3 |

**DoD:** `ledger-service` correctly applies/rejects transactions and
flags blacklisted deposits, fully testable in isolation (no other
unbuilt service needed — publish a fake `deposits.confirmed` message
by hand). Estimated 5-6 PRs.

## Milestone 2 — Deposit flow, end to end

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 2.1 | `chain-watcher` | `deposit-flow.md` | Postgres schema (watermark, `observed_transfers`), Sepolia RPC polling, known-address set (Redis), Confirmation Depth = 32 blocks, publishes `deposits.detected`/`deposits.confirmed` with the exact payload the doc's diagram specifies | 0.1 |
| 2.2 | `ledger-service` | 2.1's real topic | Wire M1.4's consumer scaffold to the live topic | 1.5, 2.1 |
| 2.3 | `sweeper` | `deposit-flow.md` invariants (Gas Top-Up, Sweep idempotency #8, #10, #12, #13) | Consumes `deposits.confirmed`, independent compliance check, Gas Top-Up (0.001 ETH) via `wallet-service /v1/sign/hot-wallet` with idempotency key `gas-topup:{chain_id}:{tx_hash}:{log_index}` (**PROPOSED**), fresh-balance-read before signing, sweeps to Hot Wallet via `wallet-service /v1/sign/user-address` | 0.3, 0.5, 2.1 |
| 2.4 | `history-service` | `deposits.confirmed`/`deposits.detected` | Minimal MongoDB consumer, denormalized read model | 2.1 |
| 2.5 | `api-gateway` | — | `GET /deposit-address` (proxies `wallet-service`), `GET /balance` (proxies `ledger-service`) | 1.3 |
| 2.6 | all of the above | — | e2e test against a local Sepolia fork (Anvil): simulate a deposit, assert balance updates | 2.1–2.5 |

**DoD:** a user can request a deposit address, send testnet ETH, and
see their balance update after 32 blocks — first real demo. Estimated
6 PRs.

## Milestone 3 — Withdrawal flow, end to end

| Task | Module | Input | Output | Depends on |
|---|---|---|---|---|
| 3.1 | `withdrawal-service` | `withdrawal-flow.md` invariant #11 | Schema (state machine `requested→validated→reserved→signed→broadcast→confirmed`), `POST /withdrawals` idempotent creation (`INSERT...ON CONFLICT DO NOTHING` first, before anything else) | 0.1 |
| 3.2 | `withdrawal-service` | invariant #10, whitelist (below), `compliance-screening.md` | Validation: min-withdrawal-amount, whitelist-active check, blacklist check — all before any ledger reservation | 3.1, 3.6 |
| 3.3 | `withdrawal-service` | invariants #1–#6, #16 | Reserve (via `ledger.apply()`) → sign (via `wallet-service /v1/sign/hot-wallet`, passing the Withdrawal Request's own `idempotency_key`) → broadcast → poll to Withdrawal Confirmation Depth = 64 blocks | 3.2, 0.3, 0.4 |
| 3.4 | `withdrawal-service` | invariants #7–#9 | Nonce Gap detection, `disputed` state, freeze via `wallet-service`, operator-resolution stub (manual DB action, no UI yet) | 3.3, 0.4 |
| 3.5 | `withdrawal-service` | latest idempotency-gap fix | Compensating Entry using the deterministic `{idempotency_key}:refund` key | 3.3 |
| 3.6 | `withdrawal-service` | new Withdrawal Whitelist section | Add (pending → active after 24h cooldown) / remove (instant) whitelist entries | 3.1 |
| 3.7 | `api-gateway` | — | `POST /withdrawals`, `POST /whitelist`, `DELETE /whitelist/:id` | 3.2, 3.6 |
| 3.8 | all of the above | — | e2e test incl. a forced Nonce Gap (reorg simulation on the local fork) | 3.1–3.5 |

**DoD:** a user can withdraw to a whitelisted address and see it
confirm; a simulated reorg correctly freezes and disputes affected
withdrawals. Estimated 8 PRs.

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
| 0 — Foundations | 5 | 5 |
| 1 — Ledger core | 5-6 | 10-11 |
| 2 — Deposit flow | 6 | 16-17 |
| 3 — Withdrawal flow | 8 | 24-25 |
| 4 — Tiering tooling | 3 | 27-28 |
| 5 — Compliance completion | 2 | 29-30 |

At a sustainable pace of 2-4 reviewed PRs/day, milestones 0-3 (the
"it actually works end-to-end" core) land in roughly **6-12 working
days**; 4-5 add **1-2 more**. Not included: Internal Transfer (blocked
on a design doc — see above) and the observability phase (roadmap step
3, deliberately deferred by several open questions already).

## Integration contracts

A companion doc consolidating the exact Kafka payload shapes and REST
endpoints referenced piecemeal across the five design docs (so
producer and consumer code can't drift) is being drafted separately by
the Solution Architect: `docs/design/integration-contracts.md`.
