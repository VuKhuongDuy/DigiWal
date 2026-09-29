# Integration contracts

## Problem

`ledger.md`, `deposit-flow.md`, `withdrawal-flow.md`, `wallet-tiering.md`
and `compliance-screening.md` each describe their own slice of the wire
contracts between services — a payload shape in a comment on one
sequence-diagram arrow here, a request shape spelled out in prose there.
Multiple dev PRs are about to be built against these contracts in
parallel (`docs/IMPLEMENTATION_PLAN.md`'s milestones 0-5), each by a
separate session with no shared memory of the others' assumptions. If
`chain-watcher`'s dev and `ledger-service`'s dev each guess a slightly
different field name or type for the same Kafka message, that's a
runtime integration bug discovered late, not a docs nit caught in
review. This doc is a single lookup table for every topic, endpoint, and
synchronous call already implied across the five docs, so producer and
consumer code can be written from the same source without re-deriving it
from a diagram comment each time.

This doc **decides nothing new architecturally**. Every entry either:
- **cites** the doc + section it was already decided in (pulled out
  verbatim where a shape was already given), or
- is marked **PROPOSED HERE** — a concrete shape consistent with the
  citing doc's stated invariants, but never actually written down
  anywhere before this doc. Treat every PROPOSED HERE entry as a draft
  for your sign-off, not a decision.
- is marked **GAP** — something named or implied elsewhere that this doc
  deliberately does not fill in, because doing so would mean inventing
  architecture, which isn't this doc's job.

## How to read the tables

- **Producer / Consumer(s)** — service names exactly as used in
  `docs/ARCHITECTURE.md`'s service table.
- **Field types** — all `amount` fields below are specified as **decimal
  string**, not a JSON number, for every wire payload in this doc — see
  Open Questions #1 for why, since none of the five docs actually says
  this and it's a real correctness risk if left to each dev's guess.
- Every entry states its **idempotency key** inline (also collected in
  one place in §4), since `docs/TEAM_OPERATING_MODEL.md`'s whole reason
  for existing is to stop exactly this kind of cross-cutting,
  safety-critical detail from drifting.

---

## 1. Kafka topics

### `deposits.detected`

| | |
|---|---|
| Producer | `chain-watcher` |
| Consumer(s) | `history-service` |
| Idempotency key | `(chain_id, tx_hash, log_index)` |
| Source | Topic name: `docs/ARCHITECTURE.md` service-bus list. Semantics ("informational only, lets `history-service` show the user an incoming/pending deposit"): `deposit-flow.md`, Ubiquitous language > *Deposit Detected*. **Payload: PROPOSED HERE** — never spelled out anywhere; `deposit-flow.md`'s diagram (line "CW->>Kafka: publish deposits.detected") has no field comment at all, unlike `deposits.confirmed`'s arrow. |

Payload (proposed):

| Field | Type | Notes |
|---|---|---|
| `chain_id` | integer | EVM chain id (e.g. `11155111` for Sepolia — `deposit-flow.md`, "Decided: chosen testnet is Sepolia") |
| `tx_hash` | string (hex) | |
| `log_index` | integer | See Open Questions #2 — native-transfer convention undefined |
| `asset_id` | uuid | FK into the Asset Registry (`ledger.md`) — invariant #2 says only registered assets ever reach this point, so it's always resolvable here |
| `from_address` | string (hex) | Needed downstream for compliance screening even at the detected stage if any future consumer wants it; matches `deposits.confirmed`'s field set for consistency |
| `to_address` | string (hex) | The Watched Address (`deposit-flow.md`) |
| `amount` | decimal string, minor units | See "How to read the tables" above |
| `block_number` | integer | Block the transfer was included in (pre-confirmation-depth) |
| `detected_at` | timestamptz (ISO 8601) | |

### `deposits.confirmed`

| | |
|---|---|
| Producer | `chain-watcher` |
| Consumer(s) | `ledger-service`, `sweeper` (each independently — `compliance-screening.md` invariant #2 says `sweeper` must not rely on `ledger-service`'s decision), `history-service` (inferred — not stated per-topic, but implied by the same read-model needing to move a deposit from "pending" to "confirmed"; flagged in Open Questions #3) |
| Idempotency key | `(chain_id, tx_hash, log_index)` — `ledger.md` invariant #3 (`UNIQUE(chain_id, tx_hash, log_index)` on `ledger_transactions`), backstopped by `deposit-flow.md` invariant #3 (Observed Transfer's own unique natural key + `pending → confirmed` guarded transition) |
| Source | `deposit-flow.md`, Ubiquitous language > *Deposit Confirmed* (prose) **and** the diagram arrow `CW->>Kafka: publish deposits.confirmed`. **These two disagree** — see Open Questions #1. |

Payload (using the fuller, prose version — see Open Questions #1 for the discrepancy):

| Field | Type | Notes |
|---|---|---|
| `chain_id` | integer | |
| `tx_hash` | string (hex) | |
| `log_index` | integer | |
| `asset_id` | uuid | |
| `from_address` | string (hex) | **Present in `deposit-flow.md`'s prose, absent from its diagram comment.** Required downstream regardless: `compliance-screening.md` invariants #1 and #2 both check `from_address` in the `deposits.confirmed` consumers (`ledger-service`, `sweeper`) — the field must exist on the wire or those checks are impossible. |
| `to_address` | string (hex) | |
| `amount` | decimal string, minor units | |
| `block_number` | integer | |

### `withdrawals.requested`

| | |
|---|---|
| Producer | **GAP-adjacent — PROPOSED HERE.** `withdrawal-service`, if this topic is built at all. |
| Consumer(s) | `history-service` (proposed, same reasoning as `deposits.detected`) |
| Idempotency key | `idempotency_key` (client-supplied, minted by `api-gateway` — `withdrawal-flow.md`, Ubiquitous language > *Withdrawal Request*) |
| Source | Topic name only: `docs/ARCHITECTURE.md` service-bus list. **Nothing else.** `withdrawal-flow.md`'s own sequence diagram never publishes to this topic at all — it only publishes `withdrawals.broadcast` and `withdrawals.confirmed`. See Open Questions #4 — this doc is not deciding whether/when it should be published, only proposing a shape in case it is. |

Payload (proposed, contingent on Open Questions #4 being resolved in favor of building it):

| Field | Type | Notes |
|---|---|---|
| `idempotency_key` | string (uuid) | |
| `user_id` | string | |
| `asset_id` | uuid | |
| `amount` | decimal string, minor units | |
| `to_address` | string (hex) | |
| `status` | string | `"requested"` — proposed trigger point: on transition to `reserved` (`withdrawal-flow.md` invariant #1), mirroring `deposits.detected`'s "seen but not final yet" role |
| `requested_at` | timestamptz | |

### `withdrawals.broadcast`

| | |
|---|---|
| Producer | `withdrawal-service` |
| Consumer(s) | `history-service` (inferred, not stated explicitly — see Open Questions #3) |
| Idempotency key | `idempotency_key` — same key as the Withdrawal Request throughout its lifecycle (`withdrawal-flow.md`, Ubiquitous language > *Withdrawal Request*); `withdrawal-flow.md` invariant #2 (`status='signing' WHERE status='reserved'`, guarded) is what prevents this event from ever being produced twice for the same request |
| Source | Topic named and this exact publish point shown in `withdrawal-flow.md`'s main sequence diagram (`WD->>Kafka: publish withdrawals.broadcast`) — **but the diagram carries no field-shape comment on that arrow**, unlike `deposits.confirmed`. **Payload: PROPOSED HERE.** |

Payload (proposed):

| Field | Type | Notes |
|---|---|---|
| `idempotency_key` | string (uuid) | |
| `user_id` | string | |
| `asset_id` | uuid | |
| `amount` | decimal string, minor units | |
| `to_address` | string (hex) | |
| `chain_id` | integer | |
| `tx_hash` | string (hex) | Now known — set at the `broadcast` state transition |
| `nonce` | integer | The Hot Wallet Nonce this withdrawal claimed (`withdrawal-flow.md`, Ubiquitous language > *Hot Wallet Nonce*) |
| `broadcast_at` | timestamptz | |

### `withdrawals.confirmed`

| | |
|---|---|
| Producer | `withdrawal-service` |
| Consumer(s) | `history-service` (inferred). **Not** `ledger-service` — the debit already happened synchronously at Reservation time (`withdrawal-flow.md` invariant #1), before broadcast; nothing further needs to touch the ledger on confirmation. See §3 for the synchronous `apply()` call instead. |
| Idempotency key | `idempotency_key` (same as above) |
| Source | Topic named and publish point shown in `withdrawal-flow.md`'s main sequence diagram (`WD->>Kafka: publish withdrawals.confirmed`) — again no field-shape comment. **Payload: PROPOSED HERE.** |

Payload (proposed — same shape as `withdrawals.broadcast` plus confirmation fields):

| Field | Type | Notes |
|---|---|---|
| `idempotency_key` | string (uuid) | |
| `user_id` | string | |
| `asset_id` | uuid | |
| `amount` | decimal string, minor units | |
| `to_address` | string (hex) | |
| `chain_id` | integer | |
| `tx_hash` | string (hex) | |
| `nonce` | integer | |
| `block_number` | integer | Block at which Withdrawal Confirmation Depth (64 blocks, `withdrawal-flow.md`) was crossed |
| `confirmed_at` | timestamptz | |

### `transfers.internal` — GAP, not designed

**Do not build against a payload for this topic.** `docs/ARCHITECTURE.md`
names it (`ledger-service`'s row, the `api-gateway` row, the topic list),
and `docs/IMPLEMENTATION_PLAN.md` explicitly flags Internal Transfer as
having "no aggregate, no invariants, no state machine" yet, blocked on a
`solution-architect` design note of its own. Nothing in the five design
docs this doc consolidates says what an internal transfer even is
end-to-end (does it debit/credit two Accounts directly with no Chain
Account involved? does it need its own idempotency-key minting path
distinct from withdrawals? is it synchronous or event-driven?) — guessing
a payload here would be exactly the "silently presenting a guess as
decided" failure mode this whole doc exists to prevent. Listed here only
so its absence is visible in the one place someone would look for it.

### Compliance/screening — no additional topic

Checked explicitly, per this task's instruction to look for one:
`compliance-screening.md` does **not** imply a new Kafka topic. A
Flagged Deposit is recorded synchronously, inline, by whichever service
is already consuming `deposits.confirmed` (`ledger-service` writes
`flagged_deposits`; `sweeper` just skips the sweep) — see
`compliance-screening.md`'s own sequence diagram, which shows no new
topic, only a DB write inside the existing consumers. Operator review
and Screening Resolution are likewise direct actions
(`compliance-screening.md` Open Questions #2: "no admin/ops UI yet"),
not events. Nothing to add here.

---

## 2. REST / HTTP endpoints

Path style below follows `wallet-service`'s existing, already-implemented
convention (`/v1/...`, seen in `services/wallet-service/internal/api/api.go`
and its README) rather than inventing a new one.

### `wallet-service`

| Endpoint | Status | Request | Response | Cites |
|---|---|---|---|---|
| `POST /v1/users/{userID}/deposit-address`<br>`GET /v1/users/{userID}/deposit-address` | **Already implemented** (`services/wallet-service/internal/api/api.go`) | — (path param only) | `{"user_id","chain","address"}`, 200/201/404 | Existing code, not a design doc |
| `POST /v1/sign/hot-wallet` | **PROPOSED HERE** — path is invented; the *contract* (fields + no-caller-chosen-key constraint) is decided | `{"to_address","asset_id","amount","chain_id"}` | `{"signed_raw_tx"}` (hex) | Request shape + "no parameter that lets a caller pick a derivation index": `withdrawal-flow.md` invariant #5. Nonce allocation happens inside this call, atomically: invariant #6. Task table's shorthand path `POST /sign`: `docs/IMPLEMENTATION_PLAN.md` task 0.3 — not itself a design doc, cited for continuity only. |
| `POST /v1/sign/user-address` | **PROPOSED HERE — flag if you want this different, see Open Questions #5** | `{"user_id","to_address","asset_id","amount","chain_id"}` | `{"signed_raw_tx"}` | Needed for the Sweep step: `deposit-flow.md` invariant #6 ("signed by the user's own derived key... `sweeper` asks for the signature, it never holds or derives the key itself") and its Sweep diagram (`Sweeper->>Wallet: sign(from=user address, to=Hot Wallet, ...)`). This is a **second, distinct signing capability** from the Hot Wallet one above — see Open Questions #5 for why neither source doc actually reconciles this against invariant #5's "no caller-chosen key" language. |

Both `/v1/sign/*` endpoints: **mTLS**, per `withdrawal-flow.md` Open
Questions #2 ("Decided (PR #1): mTLS").

### `api-gateway`

| Endpoint | Request | Response | Cites |
|---|---|---|---|
| `GET /v1/users/{userID}/deposit-address` | — | Proxies `wallet-service`'s response verbatim | `docs/IMPLEMENTATION_PLAN.md` task 2.5 ("`GET /deposit-address` (proxies `wallet-service`)"); underlying responsibility from `docs/ARCHITECTURE.md`'s `api-gateway` row ("get deposit address") |
| `GET /v1/users/{userID}/balances` | Optional `?asset_id=` query filter | **PROPOSED HERE**: `[{"asset_id","symbol","balance"}]` | `docs/ARCHITECTURE.md` `api-gateway` row ("get balance"); `docs/IMPLEMENTATION_PLAN.md` task 2.5. Implies a new synchronous `api-gateway → ledger-service` call — no endpoint for it exists in `ledger.md` today; see §3. |
| `GET /v1/users/{userID}/history` | Optional pagination params | **PROPOSED HERE, low confidence** — `history-service`'s own schema isn't designed (`docs/IMPLEMENTATION_PLAN.md`'s module map: "no dedicated design doc") | `docs/ARCHITECTURE.md` `api-gateway` row ("get... history") |
| `POST /v1/withdrawals` | `{"idempotency_key","user_id","asset_id","amount","to_address"}` | New/first request: `202 {"idempotency_key","status":"reserved"}` (or `"rejected"` with a reason) once Reservation completes — **PROPOSED HERE**, but "returns quickly, doesn't block on confirmation" is decided (`withdrawal-flow.md` Alternatives: "Block the HTTP response until the withdrawal confirms — Rejected"). Duplicate request (same key, row already existed): **PROPOSED HERE** — `200` + "existing request's current status" (`withdrawal-flow.md` diagram says this in prose, doesn't give an HTTP status). Insufficient funds: `422` (`withdrawal-flow.md` diagram, verbatim). | Request shape: `withdrawal-flow.md` diagram, `Gateway->>WD: POST /withdrawals` arrow, verbatim. **Validation order this endpoint must enforce, per the diagram and invariants, in this exact order:** (1) idempotent row creation first, invariant #11; (2) well-formed address / asset registered / amount ≥ `min_withdrawal_amount`, invariant #10; (3) `to_address` is an `active` Whitelist Entry, invariant #12; (4) `to_address` not on the Screening List, invariant #14 / `compliance-screening.md` invariant #3; (5) only then, the ledger Reservation, whose own overdraft check is `ledger.md` invariant #1. |
| `GET /v1/withdrawals/{idempotency_key}` | — | **GAP, flagged in Open Questions #6** — the "returns quickly, tracks asynchronously" model implies a client needs a way to poll status afterward, and no doc names one. Not building a shape for it here; flagging that one is needed. |
| `POST /v1/whitelist` | **PROPOSED HERE**: `{"user_id","address"}` | `{"id","user_id","address","status":"pending","added_at"}` | Fields: `withdrawal-flow.md`'s Whitelist Entry lifecycle diagram (`Gateway->>WD: add whitelist entry (user_id, address)`). Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. Cooldown behavior it must satisfy: invariant #13 (`pending` until Whitelist Cooldown elapses, computed on read). |
| `DELETE /v1/whitelist/{id}` | — | `204`, idempotent (no-op if already gone) | Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. "Removal is instant... no state needed for that direction": `withdrawal-flow.md`, Ubiquitous language > *Whitelist Entry*, and invariant #13. |
| Internal Transfer endpoint (e.g. `POST /v1/transfers`) | — | — | **GAP.** Same reason as the `transfers.internal` topic above — `docs/ARCHITECTURE.md` names this as an `api-gateway` responsibility, but no design doc exists for it. Not proposing a shape. |

### `ledger-service` (internal — not user-facing)

| Endpoint | Request | Response | Cites |
|---|---|---|---|
| `POST /v1/ledger/transactions` (**PROPOSED HERE** — path and transport both invented; see Open Questions #7) | `{"idempotency_key","source_type","entries":[{"account_ref":"user"\|"chain","user_id"?,"asset_id","amount"}]}` | `{"status":"applied"\|"already_applied"}` or `422 {"status":"insufficient_funds"}` | Operation semantics and the exact accept/reject/no-op branches: `ledger.md`'s `apply()` sequence diagram — the diagram itself only draws `Producer->>Ledger: apply(idempotency_key, entries[])` as a generic arrow, never stating whether this is REST, gRPC, or something else. Sum-to-zero enforcement: `ledger.md` invariant #2. Idempotent replay: invariant #3. |

---

## 3. Synchronous service-to-service calls

Per `docs/ARCHITECTURE.md`: "nothing talks to another service directly
except `api-gateway` → `wallet-service`/`withdrawal-service` synchronous
calls; everything else is event-driven" — the calls below are the
concrete instances of that carve-out, plus the ones the design docs add
on top of it (`ledger-service`'s `apply()`, which none of the docs call
out as an exception to the Kafka-only rule but which is drawn as a
direct call in every sequence diagram that uses it).

| Caller | Callee | Call | Auth | Constraint | Cites |
|---|---|---|---|---|---|
| `withdrawal-service` | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id}` → `{signed_raw_tx}` | mTLS | Hot Wallet only; **no caller-chosen key** — the request has no field that could select a derivation index, capping blast radius at "can spend from the Hot Wallet" even if `withdrawal-service` is fully compromised | `withdrawal-flow.md` invariant #5, diagram |
| `sweeper` | `wallet-service` | `POST /v1/sign/hot-wallet` — for **Gas Top-Up** (Hot Wallet → user address) | mTLS | Same Hot Wallet endpoint/constraint as above; only the destination varies | `deposit-flow.md` Sweep diagram, `Sweeper->>Wallet: sign(from=Hot Wallet, to=user address, amount=gas top-up)` |
| `sweeper` | `wallet-service` | `POST /v1/sign/user-address` — for the **Sweep** itself (user address → Hot Wallet) | mTLS | Signs as the specified user's derived key — this one *does* let the caller pick which key, by design (Sweep's whole job); recommend `wallet-service` still enforce `to_address == Hot Wallet` server-side rather than trusting the caller, to bound this endpoint's blast radius the same spirit as invariant #5 does for the other one — **this constraint is PROPOSED HERE, not stated in any doc; see Open Questions #5** | `deposit-flow.md` invariant #6, Sweep diagram |
| `withdrawal-service` (sweep-monitor loop) | `wallet-service` | `POST /v1/sign/hot-wallet` — for **Treasury Sweep** (Hot Wallet → Warm) | mTLS | Same Hot Wallet endpoint; destination is the Warm address instead of an external one | `wallet-tiering.md` diagram, `Monitor->>Wallet: sign(to=Warm address, ...)` |
| `withdrawal-service` | `ledger-service` | `POST /v1/ledger/transactions` (`apply`) — for the Reservation, and again (with the `{idempotency_key}:refund` key) for a Compensating Entry | **Not specified** — flagged in Open Questions #7 alongside the transport question | Idempotent — see §4 | `ledger.md` `apply()` diagram; `withdrawal-flow.md` invariants #1, #3 |
| `api-gateway` | `wallet-service` | Proxies `GET/POST /v1/users/{userID}/deposit-address` | **Not specified** — inherits the general auth gap, Open Questions #8 | Pass-through | `docs/IMPLEMENTATION_PLAN.md` task 2.5 |
| `api-gateway` | `ledger-service`, `history-service`, `withdrawal-service` | Proxies balance/history/withdrawal/whitelist endpoints in §2 | **Not specified** — Open Questions #8 | Pass-through | `docs/ARCHITECTURE.md` `api-gateway` row: "Talks to: all of the above" |

---

## 4. Idempotency keys — consolidated

| Topic / Endpoint | Idempotency key | Enforced by |
|---|---|---|
| `deposits.detected` | `(chain_id, tx_hash, log_index)` | `observed_transfers` unique constraint, `deposit-flow.md` invariant #3 |
| `deposits.confirmed` | `(chain_id, tx_hash, log_index)` | Same, plus `ledger_transactions` `UNIQUE(chain_id, tx_hash, log_index)`, `ledger.md` invariant #3 |
| `withdrawals.requested` (if built — see Open Questions #4) | `idempotency_key` | `withdrawal_requests.idempotency_key`, `withdrawal-flow.md` invariant #11 |
| `withdrawals.broadcast` | `idempotency_key` | Same; also naturally becomes unique on `(chain_id, tx_hash)` once broadcast, since invariant #2 guarantees at-most-once signing |
| `withdrawals.confirmed` | `idempotency_key` | Same |
| `transfers.internal` | **GAP** — not designed | — |
| `POST /v1/withdrawals` | `idempotency_key`, client-supplied in the request body | `UNIQUE(idempotency_key)` on `withdrawal_requests`, `INSERT ... ON CONFLICT DO NOTHING`, `withdrawal-flow.md` invariant #11 |
| `POST /v1/sign/hot-wallet` | **None at the endpoint itself.** Every call allocates a fresh nonce (invariant #6). What prevents a double-spend is the *caller's* state machine: `withdrawal-service` only calls this once per request, guarded by `status='signing' WHERE status='reserved'` (invariant #2) | `withdrawal-flow.md` invariants #2, #6 |
| `POST /v1/sign/user-address` (Sweep) | **None; idempotent by construction instead of by key** — `sweeper` always re-reads the address's live balance immediately before signing (invariant #9), so a retried call signs whatever's actually left (near-zero after a successful earlier attempt), never a stale amount | `deposit-flow.md` invariants #7, #9 |
| `POST /v1/ledger/transactions` (`apply`) | `idempotency_key` (deposit: `(chain_id, tx_hash, log_index)`; withdrawal/refund: client/`withdrawal-service`-supplied key, `{idempotency_key}:refund` for compensations) | `ledger_transactions.idempotency_key` unique, `ON CONFLICT DO NOTHING`, `ledger.md` invariant #3; refund key derivation: `withdrawal-flow.md`, Ubiquitous language > *Compensating Entry* |
| `POST /v1/whitelist` | **PROPOSED HERE** — no key specified anywhere; proposing `UNIQUE(user_id, address)` so re-adding an already-`pending`/`active` entry no-ops rather than creating a second cooldown clock | None currently — flagged in Open Questions #9 |
| `DELETE /v1/whitelist/{id}` | Naturally idempotent — deleting a non-existent id is a no-op, not an error | — |

---

## Open questions

1. **`deposits.confirmed`'s payload has two different shapes in the same
   source doc.** `deposit-flow.md`'s Ubiquitous language section
   (*Deposit Confirmed*) includes `from_address`; its own sequence
   diagram's arrow comment omits it. I've treated the fuller, prose
   version as canonical here because `compliance-screening.md`'s
   invariants #1 and #2 both require `from_address` to be checkable by
   `ledger-service` and `sweeper` — without it those checks are
   impossible. Please confirm this is a diagram omission (should be
   fixed in `deposit-flow.md`) and not a sign the field was deliberately
   dropped for a reason I'm not seeing.
2. **`log_index` convention for native-coin deposits is undefined.**
   `ledger.md` explains why `log_index` matters for ERC-20 transfers
   (one tx, several `Transfer` events) but a native-coin transfer has no
   log at all. What value goes in the `(chain_id, tx_hash, log_index)`
   tuple for a native deposit — a sentinel like `-1`, always `0`, or
   something else? This affects the uniqueness constraint's actual
   behavior for native deposits and should be pinned down before
   `chain-watcher`/`ledger-service` are built.
3. **`history-service` as a consumer of most topics here is inferred,
   not stated per-topic.** Only `deposits.detected`'s role
   ("lets `history-service` show...") is explicit in any design doc.
   I've extended the same inference to `deposits.confirmed`,
   `withdrawals.broadcast`, and `withdrawals.confirmed` because a
   read-model showing transaction history needs all of them to move a
   row through its own pending → confirmed states — but no doc actually
   says `history-service` consumes those three. Confirm, since this
   determines whether `history-service`'s first PR needs 2 topic
   schemas or 4.
4. **Should `withdrawals.requested` be built at all?** It's named in
   `docs/ARCHITECTURE.md`'s topic list, but `withdrawal-flow.md`'s own
   design — invariants, diagram, everything — never produces or
   consumes it. Either it's a leftover from an earlier pass over
   `ARCHITECTURE.md` that should be dropped from that doc, or it's a
   real gap in `withdrawal-flow.md` that should have been designed and
   wasn't. I've proposed a shape (publish at the `reserved` transition)
   in case you want it kept, but this is a "should this exist" question,
   not just a "what shape" one.
5. **Two wallet-service signing endpoints, or one endpoint with two
   modes?** `withdrawal-flow.md` invariant #5 is explicit that its
   signing endpoint has "no parameter that lets a caller pick a
   derivation index" — the entire security argument for that invariant
   (a compromised `withdrawal-service` is capped at draining the Hot
   Wallet) depends on that being true. But `deposit-flow.md` invariant
   #6 requires `sweeper` to get a signature *from a specific user's
   derived key* for the Sweep step — which is, definitionally, a
   caller-chosen key. `deposit-flow.md` waves at this by saying Sweep
   reuses "the signing endpoint (mTLS, same as `withdrawal-service` ↔
   `wallet-service`)" without addressing that it needs different
   semantics. I've modeled these as two separate endpoints
   (`/v1/sign/hot-wallet` vs. `/v1/sign/user-address`) with the
   user-address one further constrained (proposed, not decided) to only
   ever sign transactions whose destination is the Hot Wallet, so a
   compromised `sweeper` is still capped at "can move already-deposited,
   not-yet-swept user funds into the Hot Wallet" rather than anywhere.
   This is a real design decision, not a naming nit — want it done this
   way, or differently?
6. **No documented way to poll a Withdrawal Request's status after the
   initial `POST /v1/withdrawals` response.** `withdrawal-flow.md`
   explicitly decided the response returns quickly rather than blocking
   on confirmation, which implies a client needs some way to check
   status afterward (a `GET`, a webhook, relying on `history-service`
   entirely) — none of the five docs say which. Flagging as a gap in
   `withdrawal-flow.md` surfaced by trying to write its full wire
   contract, not something I'm resolving here.
7. **Transport for `ledger-service`'s `apply()` and both proposed sign
   endpoints is assumed to be REST**, matching `wallet-service`'s only
   currently-implemented endpoint. No design doc actually says REST vs.
   gRPC vs. something else for any service-to-service call — every
   sequence diagram uses a generic arrow. Worth pinning down once, in
   one place, since it's the kind of infra choice `CLAUDE.md` asks not
   to default on silently: confirm REST is fine, or say otherwise.
8. **Every `api-gateway` endpoint in this doc assumes a `user_id` is
   already known/authenticated on the request.** This isn't a new gap —
   `withdrawal-flow.md` Open Questions #7 already flagged that no
   user-auth design exists anywhere in this project — but it's worth
   repeating here since this doc is the first place *every* user-facing
   endpoint is listed in one spot, and every single one of them
   currently just takes `user_id` as a trusted request field.
9. **`amount` as a decimal string on the wire, not a JSON number.**
   `ledger.md`'s schema stores amounts as Postgres `bigint` (fits
   int64), but that's a *storage* choice, not a wire-format one, and it
   doesn't obviously hold for every asset: an 18-decimal ERC-20 token
   with a plausible real-world supply can exceed `bigint`/int64 range in
   minor units well before hitting anything exotic. No design doc
   addresses wire-level numeric representation at all. I've defaulted
   every `amount` field in this doc to a decimal string to sidestep
   JSON's lack of a real integer type and any int64 overflow risk,
   rather than silently picking a JSON number and hoping it's always
   small enough. Flag if you'd rather standardize on something else
   (e.g. a fixed-point struct, or confirm minor units will always fit
   int64 for every asset you intend to register and a plain number is
   fine).
10. **`POST /v1/whitelist`'s idempotency key is proposed, not decided**
    (`UNIQUE(user_id, address)`, §4 above) — confirm re-adding an
    existing entry should no-op rather than error or restart its
    cooldown.
