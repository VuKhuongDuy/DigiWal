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
  Open Questions #9 for why, since none of the five docs actually says
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
| `log_index` | integer | **Decided:** `-1` for native-coin deposits (sentinel — no log exists to index); the real, always ≥0 log index for ERC-20 deposits. Resolves Open Questions #2. See `ledger.md` and `deposit-flow.md`'s natural-key definitions. |
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
| Consumer(s) | `ledger-service`, `sweeper` (each independently — `compliance-screening.md` invariant #2 says `sweeper` must not rely on `ledger-service`'s decision), `history-service` (**Decided:** yes — one of the four topics `history-service` consumes, resolving Open Questions #3) |
| Idempotency key | `(chain_id, tx_hash, log_index)` — `ledger.md` invariant #3 (`UNIQUE(chain_id, tx_hash, log_index)` on `ledger_transactions`), backstopped by `deposit-flow.md` invariant #3 (Observed Transfer's own unique natural key + `pending → confirmed` guarded transition) |
| Source | `deposit-flow.md`, Ubiquitous language > *Deposit Confirmed* (prose) **and** the diagram arrow `CW->>Kafka: publish deposits.confirmed`. **Decided: prose was canonical, diagram was the bug** — `deposit-flow.md`'s diagram now carries `from_address` on that arrow too, matching the prose. Resolves Open Questions #1. |

Payload (now matches both `deposit-flow.md`'s prose and its diagram — see Open Questions #1, resolved):

| Field | Type | Notes |
|---|---|---|
| `chain_id` | integer | |
| `tx_hash` | string (hex) | |
| `log_index` | integer | |
| `asset_id` | uuid | |
| `from_address` | string (hex) | **Decided:** present in both `deposit-flow.md`'s prose and its diagram now (Open Questions #1, resolved — the diagram was the one that was wrong). Required downstream regardless: `compliance-screening.md` invariants #1 and #2 both check `from_address` in the `deposits.confirmed` consumers (`ledger-service`, `sweeper`) — the field must exist on the wire or those checks are impossible. |
| `to_address` | string (hex) | |
| `amount` | decimal string, minor units | |
| `block_number` | integer | |

### `withdrawals.requested` — considered, decided against

**Decided: not built** (resolves Open Questions #4). This topic name
appeared in `docs/ARCHITECTURE.md`'s original Kafka topic list, but
`withdrawal-flow.md`'s own design — invariants, diagram, every state
transition — never actually produces or consumes it; it only ever
publishes `withdrawals.broadcast` and `withdrawals.confirmed`. Building
it would have meant inventing a producer, a trigger point, and a
consumer contract with no design backing any of the three.
`docs/ARCHITECTURE.md`'s topic list has been updated to drop it. The
"let a client (or `history-service`) check a withdrawal's status early,
before it broadcasts" need this topic might have served is instead
covered by `GET /v1/withdrawals/{idempotency_key}` (§2, decided below)
— a pull-based status check on the one already-existing request record,
rather than a second event stream for something no consumer was ever
designed to read.

### `withdrawals.broadcast`

| | |
|---|---|
| Producer | `withdrawal-service` |
| Consumer(s) | `history-service` (**Decided** — one of the four topics it consumes; resolves Open Questions #3) |
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
| Consumer(s) | `history-service` (**Decided**, resolves Open Questions #3). **Not** `ledger-service` — the debit already happened synchronously at Reservation time (`withdrawal-flow.md` invariant #1), before broadcast; nothing further needs to touch the ledger on confirmation. See §3 for the synchronous `apply()` call instead. |
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
| `POST /v1/users/{userID}/deposit-address`<br/>`GET /v1/users/{userID}/deposit-address` | **Already implemented** (`services/wallet-service/internal/api/api.go`) | — (path param only) | `{"user_id","chain","address"}`, 200/201/404 | Existing code, not a design doc |
| `POST /v1/sign/hot-wallet` | **PROPOSED HERE** — path is invented; the *contract* (fields + no-caller-chosen-key constraint) is decided; the gas-construction approach below is **PROPOSED, pending Tech Lead sign-off** (new, see Open Questions #11) | `{"to_address","asset_id","amount","chain_id","idempotency_key"}`. **Decided** (Tech Lead review): `idempotency_key` is required — the same key the Withdrawal Request already carries throughout its lifecycle for that caller (`withdrawal-flow.md`, Ubiquitous language > *Withdrawal Request*), or the caller-specific PROPOSED key for the other two callers (see §3, §4). **PROPOSED HERE**, pending sign-off (Open Questions #11): no gas-parameter field at all — `wallet-service` is proposed to construct the transaction itself (fetch gas price, estimate gas limit) via its own EVM RPC client, rather than accept a caller-supplied gas price/limit. An optional `max_gas_price` field (a caller-supplied safety **ceiling**, never a dictated price) is also **PROPOSED**, as a separate yes/no from the construction-ownership question — see Open Questions #11. | `{"signed_raw_tx","nonce"}` (hex tx, integer nonce). **Decided:** `nonce` is now part of the response — the caller needs it for its own state (e.g. `withdrawals.broadcast`'s payload already carries `nonce`, §1 above). On a **repeated call with an already-seen `idempotency_key`**, returns the *identical* stored `{signed_raw_tx, nonce}` instead of allocating/signing anything new — **Decided** (Tech Lead review, `withdrawal-flow.md` invariant #16): closes a lost-response gap where a retried call would otherwise claim a second nonce and strand the first, since EVM's strict per-account nonce ordering means the stranded nonce can never confirm. | Request shape + "no parameter that lets a caller pick a derivation index": `withdrawal-flow.md` invariant #5. Nonce allocation happens inside this call, atomically: invariant #6. Idempotency-per-key requirement: invariant #16 (new). Gas-construction proposal and the `max_gas_price` option: this doc's Open Questions #11, `withdrawal-flow.md` Open Questions #8. Task table's shorthand path `POST /sign`: `docs/IMPLEMENTATION_PLAN.md` task 0.3 — not itself a design doc, cited for continuity only. |
| `POST /v1/sign/user-address` | **Decided: approved as proposed** (Open Questions #5, resolved) | `{"user_id","to_address","asset_id","amount","chain_id"}` | `{"signed_raw_tx"}` | Needed for the Sweep step: `deposit-flow.md` invariant #6 ("signed by the user's own derived key... `sweeper` asks for the signature, it never holds or derives the key itself") and its Sweep diagram (`Sweeper->>Wallet: sign(from=user address, to=Hot Wallet, ...)`). This is a **second, distinct signing capability** from the Hot Wallet one above, deliberately: the caller picks *which user key* signs (Sweep's whole job) but **never picks the destination** — `wallet-service` must enforce server-side that the destination is always the Hot Wallet, never trusting the caller's `to_address` for this endpoint. Now a standing invariant: `deposit-flow.md` invariant #7. **No idempotency key on this endpoint** — unchanged by this round of fixes; it stays idempotent-by-construction via `deposit-flow.md` invariants #8/#10 (`sweeper` always re-reads the live balance before signing), not by key, since a retried call naturally signs whatever's actually left rather than a stale amount. **The user address's own EVM nonce for this call is sourced by `wallet-service` via a live `eth_getTransactionCount(address, "pending")` RPC read at signing time — PROPOSED, `deposit-flow.md` invariant #12** — a different, simpler mechanism than the Hot Wallet's DB-allocated counter (invariant #6 above), justified there by the fact a user address never has two concurrent outbound signers the way the Hot Wallet does. |

**Both `/v1/sign/*` endpoints: mTLS**, per `withdrawal-flow.md` Open
Questions #2 ("Decided (PR #1): mTLS"). **Which caller's certificate is
allowed on which endpoint — the CN-based authorization policy that
makes this mTLS decision actually enforce anything by identity, plus
the CA/certificate-issuance design behind it — is now a fully specified,
separate doc: `docs/design/service-auth-mtls.md`.** Concretely (see that
doc's Authorization matrix): `POST /v1/sign/hot-wallet` accepts client
certs with `CN=withdrawal-service` or `CN=sweeper`; `POST
/v1/sign/user-address` accepts `CN=sweeper` only —
`withdrawal-service`'s certificate is never on that second endpoint's
allow-list. `wallet-service`'s deposit-address endpoints below are also
**PROPOSED** (`service-auth-mtls.md` Open Questions #4) to move behind
the same mTLS listener with `CN=api-gateway` required, for consistency —
not yet decided, since it touches already-shipped code.

**Note (post-`identity-auth.md`):** the `{userID}` in
`POST/GET /v1/users/{userID}/deposit-address` and the `user_id` fields
above are unchanged in shape, but the *trust* behind them changed —
every one of these calls now only ever originates from `api-gateway`,
which fills in `user_id`/`userID` with the value it resolved from the
caller's `Authorization: Bearer <api_key>` (`identity-auth.md`
invariants #3-#5), never a value asserted directly by an external
client. `wallet-service` itself is not reachable from outside the
deployment (`identity-auth.md` invariant #5), so this is a change in
who can be trusted to call it, not in the endpoint's own request/response
shape.

### `api-gateway`

**Every endpoint below except `POST /users` requires
`Authorization: Bearer <api_key>`.** `api-gateway` resolves the real
`user_id` from that key and injects it into every downstream call;
client-supplied `user_id` is no longer accepted as a request field on
any of these endpoints, whether as a path segment or a body/query
param (`identity-auth.md` invariants #3, #4). Missing/invalid header →
`401`, per `identity-auth.md`'s diagram.

| Endpoint | Request | Response | Cites |
|---|---|---|---|
| `POST /users` | — (empty body, `{}`); **no `Authorization` header** — this is the one exception (`identity-auth.md` invariant #3) | `201 {"user_id","api_key"}` — `api_key` shown once here, never returned by any endpoint afterward | `identity-auth.md`'s User/API Key creation sequence diagram, verbatim; invariant #1 (`user_id` is system-generated, never client-chosen). Included here, alongside the rest of `api-gateway`'s client-facing surface, because that's this doc's whole job (a single wire-contract lookup table) — `identity-auth.md` remains the doc of record for *why* this shape exists and its invariants. |
| `GET /v1/deposit-address` | — (no `{userID}` path segment; `user_id` comes from `Authorization: Bearer <api_key>`, not the client) | Proxies `wallet-service`'s response verbatim. `api-gateway` calls `wallet-service`'s (unchanged) `GET /v1/users/{userID}/deposit-address` (§2 `wallet-service` table above) with the authenticated `user_id` substituted in server-side. | `docs/IMPLEMENTATION_PLAN.md` task 2.5 ("`GET /deposit-address` (proxies `wallet-service`)"); underlying responsibility from `docs/ARCHITECTURE.md`'s `api-gateway` row ("get deposit address"). `{userID}` dropped from the client-facing path, `user_id` no longer client-supplied: `identity-auth.md` invariants #3, #4. |
| `GET /v1/balances` | Optional `?asset_id=` query filter; no `user_id`/`{userID}` — resolved from `Authorization: Bearer <api_key>` | **PROPOSED HERE**: `[{"asset_id","symbol","balance"}]` | `docs/ARCHITECTURE.md` `api-gateway` row ("get balance"); `docs/IMPLEMENTATION_PLAN.md` task 2.5. Implies a new synchronous `api-gateway → ledger-service` call — no endpoint for it exists in `ledger.md` today; see §3. `{userID}` path segment and client-supplied `user_id` removed per `identity-auth.md` invariants #3, #4. |
| `GET /v1/history` | Optional pagination params; no `user_id`/`{userID}` — resolved from `Authorization: Bearer <api_key>` | **PROPOSED HERE, low confidence** — `history-service`'s own schema isn't designed (`docs/IMPLEMENTATION_PLAN.md`'s module map: "no dedicated design doc") | `docs/ARCHITECTURE.md` `api-gateway` row ("get... history"). `{userID}` path segment and client-supplied `user_id` removed per `identity-auth.md` invariants #3, #4. |
| `POST /v1/withdrawals` | `{"idempotency_key","asset_id","amount","to_address"}` — no `user_id` field; `api-gateway` injects the authenticated `user_id` before forwarding to `withdrawal-service` (§3) | New/first request: `202 {"idempotency_key","status":"reserved"}` (or `"rejected"` with a reason) once Reservation completes — **PROPOSED HERE**, but "returns quickly, doesn't block on confirmation" is decided (`withdrawal-flow.md` Alternatives: "Block the HTTP response until the withdrawal confirms — Rejected"). Duplicate request (same key, row already existed): **PROPOSED HERE** — `200` + "existing request's current status" (`withdrawal-flow.md` diagram says this in prose, doesn't give an HTTP status). Insufficient funds: `422` (`withdrawal-flow.md` diagram, verbatim). | Request shape: `withdrawal-flow.md` diagram, `Gateway->>WD: POST /withdrawals` arrow, minus `user_id` — that diagram predates `identity-auth.md`; `user_id` is dropped from the client-facing body per invariant #4 and instead server-injected before the `api-gateway → withdrawal-service` call (§3, unchanged shape there). **Validation order this endpoint must enforce, per the diagram and invariants, in this exact order:** (1) idempotent row creation first, invariant #11; (2) well-formed address / asset registered / amount ≥ `min_withdrawal_amount`, invariant #10; (3) `to_address` is an `active` Whitelist Entry, invariant #12; (4) `to_address` not on the Screening List, invariant #14 / `compliance-screening.md` invariant #3; (5) only then, the ledger Reservation, whose own overdraft check is `ledger.md` invariant #1. |
| `GET /v1/withdrawals/{idempotency_key}` | — (path param only — `idempotency_key`, not a user identifier, so it's unaffected by the `{userID}` removal elsewhere) | **Decided** (resolves Open Questions #6): `{"idempotency_key","status","tx_hash"}` — `status` is the Withdrawal Request's current state-machine value (`requested`\|`validated`\|`reserved`\|`signed`\|`broadcast`\|`confirmed`\|`rejected`\|`failed`\|`disputed`, per `withdrawal-flow.md`'s state list), `tx_hash` is `null` until known (set from the `broadcast` state onward); `404` if the key doesn't exist | Needed because `POST /v1/withdrawals` returns quickly rather than blocking on confirmation (`withdrawal-flow.md` Alternatives: "Block the HTTP response until the withdrawal confirms — Rejected") — a client needs some way to check status afterward. State list: `withdrawal-flow.md`'s Withdrawal Request states paragraph and invariants #1-#9. |
| `POST /v1/whitelist` | `{"address"}` — no `user_id` field; `api-gateway` injects the authenticated `user_id` before forwarding | `{"id","user_id","address","status":"pending","added_at"}` (`user_id` still appears in the *response*, identifying whose entry this is — only the client-supplied request field is dropped) | Fields: `withdrawal-flow.md`'s Whitelist Entry lifecycle diagram (`Gateway->>WD: add whitelist entry (user_id, address)`) — `user_id` removed from the client-facing request per `identity-auth.md` invariant #4. Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. Cooldown behavior it must satisfy: invariant #13 (`pending` until Whitelist Cooldown elapses, computed on read). |
| `DELETE /v1/whitelist/{id}` | — | `204`, idempotent (no-op if already gone) | Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. "Removal is instant... no state needed for that direction": `withdrawal-flow.md`, Ubiquitous language > *Whitelist Entry*, and invariant #13. |
| Internal Transfer endpoint (e.g. `POST /v1/transfers`) | — | — | **GAP.** Same reason as the `transfers.internal` topic above — `docs/ARCHITECTURE.md` names this as an `api-gateway` responsibility, but no design doc exists for it. Not proposing a shape. Whatever shape it eventually gets, it's still bound by the same rule as every other row in this table: `Authorization: Bearer <api_key>` required, no client-supplied `user_id` (`identity-auth.md` invariants #3, #4). |

### `ledger-service` (internal — not user-facing)

| Endpoint | Request | Response | Cites |
|---|---|---|---|
| `POST /v1/ledger/transactions` (**PROPOSED HERE** — path invented; **transport decided: REST**, resolving Open Questions #7) | `{"idempotency_key","source_type","entries":[{"account_ref":"user"\|"chain","user_id"?,"asset_id","amount"}]}` | `{"status":"applied"\|"already_applied"}` or `422 {"status":"insufficient_funds"}` | Operation semantics and the exact accept/reject/no-op branches: `ledger.md`'s `apply()` sequence diagram — the diagram itself only draws `Producer->>Ledger: apply(idempotency_key, entries[])` as a generic arrow, never stating whether this is REST, gRPC, or something else. Sum-to-zero enforcement: `ledger.md` invariant #2. Idempotent replay: invariant #3. Transport: **Decided — REST**, matching `wallet-service`'s already-implemented convention. **Auth: PROPOSED** — same mTLS + CN-authorization mechanism as `wallet-service`'s signing endpoints, restricted to `CN=withdrawal-service` only; see `docs/design/service-auth-mtls.md` invariant #7 and its Authorization matrix. Not yet built (ledger-service doesn't exist yet) — recommended to be built in from the start at Milestone 1 rather than retrofitted. |

---

## 3. Synchronous service-to-service calls

Per `docs/ARCHITECTURE.md`: "nothing talks to another service directly
except `api-gateway` → `wallet-service`/`withdrawal-service` synchronous
calls; everything else is event-driven" — the calls below are the
concrete instances of that carve-out, plus the ones the design docs add
on top of it (`ledger-service`'s `apply()`, which none of the docs call
out as an exception to the Kafka-only rule but which is drawn as a
direct call in every sequence diagram that uses it).

**Auth column note:** "mTLS" below means "decided, per
`withdrawal-flow.md` Open Questions #2." Which CN is authorized for
each row, the CA behind it, and the enforcement mechanism inside the
receiving service are now specified in one place:
`docs/design/service-auth-mtls.md` — cited per-row below instead of
repeating the detail on every line.

| Caller | Callee | Call | Auth | Constraint | Cites |
|---|---|---|---|---|---|
| `withdrawal-service` | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` | mTLS — client cert `CN=withdrawal-service`, on this endpoint's allow-list (`service-auth-mtls.md` Authorization matrix) | Hot Wallet only; **no caller-chosen key** — the request has no field that could select a derivation index, capping blast radius at "can spend from the Hot Wallet" even if `withdrawal-service` is fully compromised. **`idempotency_key` = the Withdrawal Request's own `idempotency_key`** (unchanged, reused as-is — not minted fresh per call). A repeated call with this key (e.g. after a lost response) gets back the identical `{signed_raw_tx, nonce}`, never a second nonce. | `withdrawal-flow.md` invariants #5, #16, diagram; CN-authorization enforcement: `service-auth-mtls.md` invariants #2, #4 |
| `sweeper` | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` — for **Gas Top-Up** (Hot Wallet → user address) | mTLS — client cert `CN=sweeper`, also on this endpoint's allow-list | Same Hot Wallet endpoint/constraint as above; only the destination varies. **`idempotency_key` = `gas-topup:{chain_id}:{tx_hash}:{log_index}` — PROPOSED** (`deposit-flow.md` invariant #13), deterministically derived from the triggering deposit's own natural key (namespaced with a `gas-topup:` prefix so it can never collide with that deposit's ledger-side idempotency key). | `deposit-flow.md` Sweep diagram, invariant #13 (new); `withdrawal-flow.md` invariant #16; `service-auth-mtls.md` Authorization matrix |
| `sweeper` | `wallet-service` | `POST /v1/sign/user-address` — for the **Sweep** itself (user address → Hot Wallet) | mTLS — client cert `CN=sweeper` **only**; `withdrawal-service`'s certificate is explicitly **not** on this endpoint's allow-list (`service-auth-mtls.md` invariant #4) — this is what makes invariant #7's "no caller-chosen key" argument for this endpoint actually enforced, not just a request-shape convention | Signs as the specified user's derived key — this one *does* let the caller pick which key, by design (Sweep's whole job). **Decided (Open Questions #5, resolved): `wallet-service` enforces `to_address == Hot Wallet` server-side, never trusting the caller.** No idempotency key on this call — stays idempotent-by-construction (fresh balance read before signing, `deposit-flow.md` invariants #8/#10), unaffected by this round of fixes. **The user address's nonce for this call is sourced by `wallet-service` via a live `eth_getTransactionCount(address, "pending")` RPC read — PROPOSED** (`deposit-flow.md` invariant #12), not a DB-tracked counter. | `deposit-flow.md` invariants #6, #7, #12 (new); Sweep diagram; `service-auth-mtls.md` invariants #2, #4 |
| `withdrawal-service` (sweep-monitor loop) | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` — for **Treasury Sweep** (Hot Wallet → Warm) | mTLS — same `CN=withdrawal-service` certificate as `withdrawal-service`'s other calls to this endpoint (it's the same service/process, not a separate identity) | Same Hot Wallet endpoint; destination is the Warm address instead of an external one. **`idempotency_key` = a fresh UUID minted once per Treasury Sweep Attempt row, reused across retries until that sweep broadcasts — PROPOSED** (`wallet-tiering.md` invariant #6), since a Treasury Sweep has no pre-existing client request to derive a key from the way a withdrawal or a gas top-up does. | `wallet-tiering.md` invariant #6 (new), diagram; `service-auth-mtls.md` Authorization matrix |
| `withdrawal-service` | `ledger-service` | `POST /v1/ledger/transactions` (`apply`) — for the Reservation, and again (with the `{idempotency_key}:refund` key) for a Compensating Entry | **Decided: REST** (Open Questions #7, resolved). **Auth: PROPOSED** — mTLS, client cert `CN=withdrawal-service` only, same mechanism as `wallet-service`'s endpoints, extended here for consistency (`service-auth-mtls.md` invariant #7) — not yet built, since `ledger-service` doesn't exist yet | Idempotent — see §4 | `ledger.md` `apply()` diagram; `withdrawal-flow.md` invariants #1, #3; `service-auth-mtls.md` invariant #7 |
| `api-gateway` | `wallet-service` | Proxies `GET/POST /v1/users/{userID}/deposit-address` | **PROPOSED** — mTLS, client cert `CN=api-gateway`, deferred (`service-auth-mtls.md` Open Questions #4): the general "who can be trusted to call this" question is already resolved at the *user-identity* level (the `userID`/`user_id` this call carries is always the one `api-gateway` resolved from the caller's `Authorization: Bearer <api_key>`, `identity-auth.md` invariants #3-#5), but at the *service-identity* level this hop still has no client-cert check today, unlike the `/v1/sign/*` endpoints | Pass-through | `docs/IMPLEMENTATION_PLAN.md` task 2.5; `service-auth-mtls.md` Authorization matrix and Open Questions #4 |
| `api-gateway` | `ledger-service`, `history-service`, `withdrawal-service` | Proxies balance/history/withdrawal/whitelist endpoints in §2 | **PROPOSED** — same as the row above: mTLS with `CN=api-gateway`, applicable once each specific downstream endpoint is actually designed (several are still GAP/PROPOSED HERE, §2) — `service-auth-mtls.md` deliberately doesn't invent those paths just to attach a CN rule to them yet | Pass-through | `docs/ARCHITECTURE.md` `api-gateway` row: "Talks to: all of the above"; `service-auth-mtls.md` Authorization matrix |

---

## 4. Idempotency keys — consolidated

| Topic / Endpoint | Idempotency key | Enforced by |
|---|---|---|
| `deposits.detected` | `(chain_id, tx_hash, log_index)` | `observed_transfers` unique constraint, `deposit-flow.md` invariant #3 |
| `deposits.confirmed` | `(chain_id, tx_hash, log_index)` | Same, plus `ledger_transactions` `UNIQUE(chain_id, tx_hash, log_index)`, `ledger.md` invariant #3 |
| ~~`withdrawals.requested`~~ | — | **Decided against, not built** — see §1's "considered, decided against" note (resolves Open Questions #4) |
| `withdrawals.broadcast` | `idempotency_key` | Same; also naturally becomes unique on `(chain_id, tx_hash)` once broadcast, since invariant #2 guarantees at-most-once signing |
| `withdrawals.confirmed` | `idempotency_key` | Same |
| `transfers.internal` | **GAP** — not designed | — |
| `POST /v1/withdrawals` | `idempotency_key`, client-supplied in the request body | `UNIQUE(idempotency_key)` on `withdrawal_requests`, `INSERT ... ON CONFLICT DO NOTHING`, `withdrawal-flow.md` invariant #11 |
| `POST /v1/sign/hot-wallet` | **Decided (Tech Lead review): required, per-caller.** `wallet-service` persists `(idempotency_key -> nonce, signed_raw_tx)`; a repeated call with the same key returns the identical stored result instead of allocating a new nonce — closes a lost-response gap invariant #2/#6 alone didn't cover (a lost response followed by a retry would otherwise strand a nonce forever, the same failure shape as a Nonce Gap but from a different cause). Three distinct callers, three key sources: (1) `withdrawal-service` → the Withdrawal Request's own `idempotency_key`, unchanged/reused (`withdrawal-flow.md`, Ubiquitous language); (2) `sweeper` (Gas Top-Up) → **PROPOSED** `gas-topup:{chain_id}:{tx_hash}:{log_index}`, derived from the triggering deposit's natural key (`deposit-flow.md` invariant #13); (3) `withdrawal-service` sweep-monitor (Treasury Sweep) → **PROPOSED** a fresh UUID minted once per Treasury Sweep Attempt row and reused across retries (`wallet-tiering.md` invariant #6). | `withdrawal-flow.md` invariant #16 (new); `deposit-flow.md` invariant #13 (new, PROPOSED); `wallet-tiering.md` invariant #6 (new, PROPOSED) |
| `POST /v1/sign/user-address` (Sweep) | **None; idempotent by construction instead of by key** — `sweeper` always re-reads the address's live balance immediately before signing (invariant #10), so a retried call signs whatever's actually left (near-zero after a successful earlier attempt), never a stale amount. Unchanged by this round of fixes — Gap 1's idempotency-key requirement was scoped to `/v1/sign/hot-wallet`'s three callers only, not this endpoint. | `deposit-flow.md` invariants #8, #10 |
| `POST /v1/ledger/transactions` (`apply`) | `idempotency_key` (deposit: `(chain_id, tx_hash, log_index)`; withdrawal/refund: client/`withdrawal-service`-supplied key, `{idempotency_key}:refund` for compensations) | `ledger_transactions.idempotency_key` unique, `ON CONFLICT DO NOTHING`, `ledger.md` invariant #3; refund key derivation: `withdrawal-flow.md`, Ubiquitous language > *Compensating Entry* |
| `POST /v1/whitelist` | **Decided:** `UNIQUE(user_id, address)` — re-adding an already-`pending`/`active` entry no-ops rather than erroring or restarting its cooldown (Open Questions #10, resolved) | `whitelist_entries` unique constraint; now also `withdrawal-flow.md` invariant #15 |
| `DELETE /v1/whitelist/{id}` | Naturally idempotent — deleting a non-existent id is a no-op, not an error | — |

---

## Open questions

1. ~~**`deposits.confirmed`'s payload has two different shapes in the same
   source doc.** `deposit-flow.md`'s Ubiquitous language section
   (*Deposit Confirmed*) includes `from_address`; its own sequence
   diagram's arrow comment omits it. I've treated the fuller, prose
   version as canonical here because `compliance-screening.md`'s
   invariants #1 and #2 both require `from_address` to be checkable by
   `ledger-service` and `sweeper` — without it those checks are
   impossible. Please confirm this is a diagram omission (should be
   fixed in `deposit-flow.md`) and not a sign the field was deliberately
   dropped for a reason I'm not seeing.~~
   **Decided: the prose was canonical, the diagram was the bug.**
   `deposit-flow.md`'s sequence diagram has been fixed — the
   `CW->>Kafka: publish deposits.confirmed` arrow's field comment now
   includes `from_address`, matching the Ubiquitous language section and
   the payload this doc already specified above.
2. ~~**`log_index` convention for native-coin deposits is undefined.**
   `ledger.md` explains why `log_index` matters for ERC-20 transfers
   (one tx, several `Transfer` events) but a native-coin transfer has no
   log at all. What value goes in the `(chain_id, tx_hash, log_index)`
   tuple for a native deposit — a sentinel like `-1`, always `0`, or
   something else? This affects the uniqueness constraint's actual
   behavior for native deposits and should be pinned down before
   `chain-watcher`/`ledger-service` are built.~~
   **Decided: `-1`.** A native-coin transfer has no log, so `log_index =
   -1` is the sentinel value in the `(chain_id, tx_hash, log_index)`
   idempotency tuple for native deposits; ERC-20 deposits keep using the
   real log index (always >= 0). Now stated explicitly in `ledger.md`
   and `deposit-flow.md` wherever they define/reference this natural
   key.
3. ~~**`history-service` as a consumer of most topics here is inferred,
   not stated per-topic.** Only `deposits.detected`'s role
   ("lets `history-service` show...") is explicit in any design doc.
   I've extended the same inference to `deposits.confirmed`,
   `withdrawals.broadcast`, and `withdrawals.confirmed` because a
   read-model showing transaction history needs all of them to move a
   row through its own pending → confirmed states — but no doc actually
   says `history-service` consumes those three. Confirm, since this
   determines whether `history-service`'s first PR needs 2 topic
   schemas or 4.~~
   **Decided: all four.** `history-service` consumes `deposits.detected`,
   `deposits.confirmed`, `withdrawals.broadcast`, and
   `withdrawals.confirmed` — its first PR needs all four topic schemas,
   not two.
4. ~~**Should `withdrawals.requested` be built at all?** It's named in
   `docs/ARCHITECTURE.md`'s topic list, but `withdrawal-flow.md`'s own
   design — invariants, diagram, everything — never produces or
   consumes it. Either it's a leftover from an earlier pass over
   `ARCHITECTURE.md` that should be dropped from that doc, or it's a
   real gap in `withdrawal-flow.md` that should have been designed and
   wasn't. I've proposed a shape (publish at the `reserved` transition)
   in case you want it kept, but this is a "should this exist" question,
   not just a "what shape" one.~~
   **Decided: drop it.** It was a leftover in `docs/ARCHITECTURE.md`'s
   topic list, never produced or consumed by `withdrawal-flow.md`'s
   actual design. Removed from `docs/ARCHITECTURE.md`'s topic list; see
   §1's "considered, decided against" note above for the replacement —
   `GET /v1/withdrawals/{idempotency_key}` (Open Questions #6) covers
   the "check status early" need instead.
5. ~~**Two wallet-service signing endpoints, or one endpoint with two
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
   way, or differently?~~
   **Decided: approved as proposed.** Keep `/v1/sign/hot-wallet` (no
   caller-chosen key) and `/v1/sign/user-address` (caller picks the
   source user key) as two distinct endpoints. `wallet-service` must
   enforce server-side that `/v1/sign/user-address`'s destination is
   always the Hot Wallet — never trust the caller's `to_address` for
   this endpoint. This is now a standing invariant, not just a note in
   this doc: `deposit-flow.md` invariant #7. **Which caller's
   certificate may reach which of these two endpoints at all** was a
   separate, still-open gap until now — closed by
   `docs/design/service-auth-mtls.md`.
6. ~~**No documented way to poll a Withdrawal Request's status after the
   initial `POST /v1/withdrawals` response.** `withdrawal-flow.md`
   explicitly decided the response returns quickly rather than blocking
   on confirmation, which implies a client needs some way to check
   status afterward (a `GET`, a webhook, relying on `history-service`
   entirely) — none of the five docs say which. Flagging as a gap in
   `withdrawal-flow.md` surfaced by trying to write its full wire
   contract, not something I'm resolving here.~~
   **Decided: add the endpoint.** `GET /v1/withdrawals/{idempotency_key}`
   (§2, now a real, decided entry) returns the Withdrawal Request's
   current state-machine status (`requested`/`validated`/`reserved`/
   `signed`/`broadcast`/`confirmed`/`rejected`/`failed`/`disputed`, per
   `withdrawal-flow.md`'s state list) plus `tx_hash` once known.
7. ~~**Transport for `ledger-service`'s `apply()` and both proposed sign
   endpoints is assumed to be REST**, matching `wallet-service`'s only
   currently-implemented endpoint. No design doc actually says REST vs.
   gRPC vs. something else for any service-to-service call — every
   sequence diagram uses a generic arrow. Worth pinning down once, in
   one place, since it's the kind of infra choice `CLAUDE.md` asks not
   to default on silently: confirm REST is fine, or say otherwise.~~
   **Decided: REST for everything**, matching `wallet-service`'s
   already-implemented convention.
8. ~~**Every `api-gateway` endpoint in this doc assumes a `user_id` is
   already known/authenticated on the request.** This isn't a new gap —
   `withdrawal-flow.md` Open Questions #7 already flagged that no
   user-auth design exists anywhere in this project — but it's worth
   repeating here since this doc is the first place *every* user-facing
   endpoint is listed in one spot, and every single one of them
   currently just takes `user_id` as a trusted request field.~~
   **Decided: resolved by `identity-auth.md`.** `api-gateway` is the
   sole authentication point in the system: every client request to it
   except `POST /users` requires `Authorization: Bearer <api_key>`
   (`identity-auth.md` invariant #3), `api-gateway` resolves the real
   `user_id` from that key and injects it into every downstream call,
   and client-supplied `user_id` is dropped entirely as a request field
   — never read from a client's body/params again (`identity-auth.md`
   invariant #4). §2's `api-gateway` table and §3's sync-call table
   above have been updated accordingly: no `{userID}` path segment and
   no `user_id` body/query field on any client-facing endpoint;
   `user_id` still appears in the `wallet-service`/`ledger-service`
   request shapes (§2, §3), but only ever as the server-injected,
   authenticated value `api-gateway` supplies — never a value an
   external client could assert directly, since only `api-gateway` is
   reachable from outside the deployment (`identity-auth.md` invariant
   #5). **Note this resolves *user*-identity trust for these calls, a
   separate question from *service*-identity trust (which service
   process is making the call) — the latter is what
   `docs/design/service-auth-mtls.md` now covers, including for these
   same `api-gateway` hops (PROPOSED, see §3 above).**
9. ~~**`amount` as a decimal string on the wire, not a JSON number.**
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
   fine).~~
   **Decided: accepted as already specified.** Decimal string, minor
   units, for every wire payload in this doc — no shape changes needed.
10. ~~**`POST /v1/whitelist`'s idempotency key is proposed, not decided**
    (`UNIQUE(user_id, address)`, §4 above) — confirm re-adding an
    existing entry should no-op rather than error or restart its
    cooldown.~~
    **Decided: `UNIQUE(user_id, address)`.** Re-adding an existing
    pending/active entry no-ops rather than erroring or restarting its
    cooldown. Now also stated in `withdrawal-flow.md` as invariant #15.
11. **PROPOSED, pending Tech Lead sign-off: `wallet-service` owns full
    EVM transaction construction for `/v1/sign/hot-wallet`, gaining its
    own EVM RPC client.** Raised by Tech Lead review of tasks 0.3/0.4.
    Previously the request shape (`{to_address, asset_id, amount,
    chain_id}`) had no gas parameters and no doc said how they'd be
    sourced — a real, unresolved question, not an oversight to paper
    over. Two options: (a) **recommended here** — `wallet-service`
    fetches current gas price and estimates gas limit itself via a new
    EVM testnet RPC dependency (Sepolia, per `deposit-flow.md`'s chain
    choice), so the caller supplies business intent only
    (`to_address, asset_id, amount, chain_id, idempotency_key`) and can
    never influence how the tx is built — consistent with invariant #5's
    existing "caller can't pick a derivation index" spirit, extended to
    "caller can't pick the gas price either"; or (b) the caller
    (`withdrawal-service`/`sweeper`) estimates gas itself and passes
    fully-specified gas parameters for `wallet-service` to sign as
    given — architecturally simpler, no new RPC dependency for
    `wallet-service`, but weakens that security property and duplicates
    gas-estimation logic across every caller. Recommending (a).
    `docs/ARCHITECTURE.md`'s `wallet-service` row has been updated to
    list "EVM testnet RPC" as a dependency, flagged there as PROPOSED —
    this is a real addition to that service's footprint (today it's
    Postgres + Redis only), not something to silently assume. **Also
    open, as a separate yes/no within this same proposal:** whether to
    accept an optional caller-supplied `max_gas_price` field as a safety
    ceiling (bounds cost exposure without dictating the actual price
    used) — recommending yes, since it costs little and gives callers a
    lever without reopening who controls tx construction, but flagging
    it as separable in case you'd rather keep v1's request minimal and
    defer this to the observability phase. See `withdrawal-flow.md`
    invariant #5 and Open Questions #8 for the same flag from that doc's
    side. **Addendum, from Dev feedback while implementing `sweeper`
    (`deposit-flow.md` invariant #14, its Open Questions #9):** this
    same option (a) vs (b) choice also determines how a native-coin
    Sweep avoids signing away its own gas money — if `wallet-service`
    owns construction, it's the natural place to compute `balance −
    actual gas cost` too, using the exact cost it's about to charge
    rather than an estimate that could go stale by broadcast time. One
    more real argument for (a), not a separate question to resolve.
12. **New, from `docs/design/service-auth-mtls.md`: which internal hops
    get CN-based authorization enforcement now vs. later.** That doc
    recommends `wallet-service`'s two `/v1/sign/*` endpoints ship the
    CN check immediately (this is what the two flow docs' blast-radius
    arguments depend on); `ledger-service`'s `apply()` gets the same
    treatment built in at Milestone 1 (not retrofitted); and every
    `api-gateway → *` proxied hop gets it only once each specific
    downstream endpoint is itself designed (several are still
    GAP/PROPOSED HERE in §2 above). Flagging here too since it directly
    affects this doc's own §3 Auth column, several of whose rows are now
    marked PROPOSED rather than silently assumed. See that doc's Open
    Questions #1-#5 for the specific decisions still pending.
