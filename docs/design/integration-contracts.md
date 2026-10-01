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
| `user_id` | string | **New, fixes a real gap (Tech Lead review):** the user this Watched Address belongs to, resolved by `chain-watcher` (`deposit-flow.md`'s new Address Resolution term) before this event is ever published. Without it `history-service` has no way to attribute a pending deposit to the right user's feed. |
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
| `user_id` | string | **New, fixes a real gap (Tech Lead review):** without this, `ledger-service` has `to_address` and nothing else — `ledger.md`'s Account is keyed by `(user_id, asset)`, never by address, so there was no way to know whose Account to credit. Resolved by `chain-watcher` (`deposit-flow.md`'s Address Resolution) the same way as `deposits.detected` above, same field. |
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
| `withdrawal_id` | string (uuid) | **Decided** (`withdrawal-flow.md` invariant #20): server-minted, globally unique; the Kafka message key and consumer dedup key |
| `idempotency_key` | string (uuid) | The client's key — unique per user only (Decided, invariant #20); informational |
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
| Consumer(s) | `history-service` (**Decided**, resolves Open Questions #3). **Not** `ledger-service`, but not for the reason this row used to give — **the debit does happen at confirmation now**, not at Reservation time (revised: `ledger.md`'s Lock/Settle/Release split, `withdrawal-flow.md` invariant #3). `withdrawal-service` calls `ledger-service`'s `POST /v1/ledger/locks/{key}/settle` **synchronously, before** publishing this event, not by having `ledger-service` consume it off Kafka — so by the time any consumer sees `withdrawals.confirmed`, the Settle has already happened; this event itself carries no ledger side effect. See §2/§3 for the synchronous `settle()` call. |
| Idempotency key | `idempotency_key` (same as above) |
| Source | Topic named and publish point shown in `withdrawal-flow.md`'s main sequence diagram (`WD->>Kafka: publish withdrawals.confirmed`) — again no field-shape comment. **Payload: PROPOSED HERE.** |

Payload (proposed — same shape as `withdrawals.broadcast` plus confirmation fields):

| Field | Type | Notes |
|---|---|---|
| `withdrawal_id` | string (uuid) | **Decided** (`withdrawal-flow.md` invariant #20): server-minted, globally unique; the Kafka message key and consumer dedup key |
| `idempotency_key` | string (uuid) | The client's key — unique per user only (Decided, invariant #20); informational |
| `user_id` | string | |
| `asset_id` | uuid | |
| `amount` | decimal string, minor units | |
| `to_address` | string (hex) | |
| `chain_id` | integer | |
| `tx_hash` | string (hex) | |
| `nonce` | integer | |
| `block_number` | integer | Block at which Withdrawal Confirmation Depth (64 blocks, `withdrawal-flow.md`) was crossed |
| `confirmed_at` | timestamptz | |

### `withdrawals.failed`

**New — Decided (Tech Lead) as a requirement; payload, version and
consumer details PROPOSED here** (`withdrawal-flow.md` invariant #24).

| | |
|---|---|
| Producer | `withdrawal-service`, from the same outbox as the other withdrawal events, in the same transaction as every guarded `→ failed` transition |
| Consumer(s) | `history-service` — sets the withdrawal document's status to `failed` (terminal). Note: `failed` can follow `confirmed` (the `disputed` → abandoned path, `withdrawal-flow.md` invariant #9), so `history-service`'s forward-only rule must allow `confirmed → failed` for this event. Future notifications. |
| Idempotency key | `withdrawal_id` (Kafka message key; a request fails at most once) |
| Not published for | `rejected` (PROPOSED, `withdrawal-flow.md` Open Questions #23 — the client already got the `422` synchronously) |

Payload (version 1, same `version` field and `pkg/events` conventions as the others):

| Field | Type | Notes |
|---|---|---|
| `withdrawal_id` | string (uuid) | Kafka key, dedup key |
| `idempotency_key` | string | The client's key (unique per user) |
| `user_id` | string | |
| `asset_id` | uuid | |
| `amount` | decimal string, minor units | |
| `to_address` | string (hex) | |
| `chain_id` | integer | |
| `reason` | string | `sign_refused`, `tx_reverted`, `stale_request`, `stale_reservation`, `replaced_or_dropped`, `destination_not_whitelisted`, `destination_on_screening_list`, or a Compensating-Entry reason (task 3.5) |
| `revert_reason` | string, optional | Only for `tx_reverted`: decoded revert string / custom error, or raw revert data hex |
| `tx_hash` | string (hex), optional | The mined (reverted) tx, or the latest known tx; absent if never signed |
| `nonce` | integer, optional | Absent if never signed |
| `failed_at` | timestamptz | |

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
| `POST /v1/users/{userID}/deposit-address`<br/>`GET /v1/users/{userID}/deposit-address` | **Already implemented** (`services/wallet-service/internal/api/api.go`) — **semantics now stated explicitly (Tech Lead review):** `POST` is idempotent get-or-create (returns the existing address if one exists, otherwise derives and persists a new one); `GET` is read-only and returns `404` if the user has no address yet. **Decided:** `api-gateway`'s client-facing `GET /v1/deposit-address` (below) always calls `wallet-service`'s `POST`, never `GET` — since `POST` is already idempotent get-or-create, there's no need for a separate GET-then-POST-on-404 round trip; every call self-provisions on first use and is safe to repeat. | — (path param only) | `{"user_id","chain","address"}`, 200/201/404 (`404` only possible on the `GET` variant, which `api-gateway` never calls) | Existing code, not a design doc |
| `GET /v1/addresses/{address}` (**PROPOSED HERE** — new, fixes a real gap: Tech Lead review of `deposit-flow.md` found neither Kafka deposit event carried a `user_id`, and this is the reverse-lookup that closes it) | — (path param only — the on-chain address, reverse direction from the endpoint above) | `200 {"user_id","chain"}` or `404` (not a Watched Address at all) | `deposit-flow.md`'s new Address Resolution term and rewritten invariant #1: `chain-watcher`'s cache-miss fallback when its Redis `address → user_id` hash (populated at `CreateAddress` time) doesn't have an entry. **Second caller (Decided, Tech Lead — `withdrawal-flow.md` invariant #18, Open Questions #9a): `withdrawal-service`** uses it to reject a withdrawal (and a whitelist entry) whose address is one of ours: `200` = reject, `404` = continue, anything else = fail closed; live call, no negative cache. Read-only, naturally idempotent — no idempotency key needed (§4). Backed by `wallet_addresses.address`, already `UNIQUE`-indexed (`services/wallet-service/migrations/000001_baseline_schema.up.sql`), so no schema change — only a new query and handler. |
| `GET /v1/wallet/hot-address` (**PROPOSED HERE** — new, fixes a real gap: Tech Lead review found no documented way for `sweeper` or `withdrawal-service`'s Treasury Sweep monitor to learn the Hot Wallet's own address other than a locally-configured constant that can silently drift from what `wallet-service` actually derived) | — (no params) | `200 {"address"}` — the address `wallet-service` derived from its `hot_wallet` seed at startup (`wallet-tiering.md`'s Hot Wallet definition) | `deposit-flow.md`'s new Hot Wallet Address Discovery term and invariant #15; `wallet-tiering.md` invariant #8. Not sensitive data — the Hot Wallet's address is public on-chain information the instant it sends its first transaction — but kept behind the same mTLS listener as every other internal endpoint for consistency, not because the value itself needs confidentiality. |
| `GET /v1/wallet/system-addresses` (**Decided (Tech Lead)** — new, `wallet-tiering.md` invariant #9, `withdrawal-flow.md` Open Questions #10) | — (no params) | `200 {"chain_id","hot","warm"}` — `hot` derived from the `hot_wallet` seed (same value as `GET /v1/wallet/hot-address`), `warm` from the operator file `infra/treasury.yaml`, validated at `wallet-service` startup (missing/malformed → startup fails) | mTLS, `CN=withdrawal-service` only. Sits **beside** `GET /v1/wallet/hot-address`, which is unchanged for `sweeper`/`chain-watcher`. Read-only, no idempotency key. |
| `GET /v1/sign/hot-wallet/{key}` (**PROPOSED HERE** — new, `withdrawal-flow.md` invariant #22, task 3.13) | — (path param: the caller's own Hot Wallet key, e.g. `withdrawal:{withdrawal_id}`) | `200 {"nonce","signed_raw_tx","tx_hash","replacements":[{"replacement_seq","tx_hash","kind"}]}` or `404` (never signed, or not this caller's key) | Read-only, diagnostics only — **not** a basis for Releasing (a sign in flight can land after a `404`). Scoped by caller CN (`caller_cn` column). mTLS, `CN=withdrawal-service` only. |
| `POST /v1/sign/hot-wallet/{key}/void` (**PROPOSED HERE** — new, invariant #22) | — | `200 {"status":"voided"}` or `200 {"status":"already_signed","nonce","signed_raw_tx","tx_hash"}`; `409` another caller's key | Atomic under the same lock as signing: returns the signature, or writes a tombstone after which `POST /v1/sign/hot-wallet` with that key returns **`410`** and allocates nothing. Idempotent. The only safe basis for the Reaper to Release a `reserved` request. mTLS, `CN=withdrawal-service` only. |
| `POST /v1/sign/hot-wallet/{key}/replace` (**PROPOSED HERE** — new, Fee Bump, invariant #21) | `{"replacement_seq","max_gas_price"?}` | `200 {"signed_raw_tx","tx_hash","nonce","replacement_seq","max_fee_per_gas","max_priority_fee_per_gas"}`; `404` never signed; `409` seq not last+1, or another caller's key; `422` fee cap exceeded; `503` Nonce Gap freeze covers the nonce | Same nonce, rebuilt from the stored original (no field can change destination/amount/data); fees = max(network, previous × 1.125) on both fee fields, capped by `max_gas_price` and a `wallet-service` config ceiling. Persisted in `hot_wallet_replacements` (PK `(key, replacement_seq)`); same seq replays. Allocates no nonce. mTLS, `CN=withdrawal-service` only. |
| `POST /v1/wallet/nonce-gap` (**Decided (Tech Lead)** — new, `withdrawal-flow.md` invariant #19, M3 task 3.4) | `{"chain_id","from_nonce"}` | `200 {"chain_id","frozen_from_nonce","frozen_at"}` — the *effective* freeze (`LEAST` of existing and requested); `400` malformed; `404` no Hot Wallet nonce row for the chain; `422` `from_nonce >= next_nonce` | Sets the Nonce Gap freeze `wallet-service` already stores (`hot_wallet_nonces.frozen_from_nonce`); while set, `POST /v1/sign/hot-wallet` refuses new signatures with `503` (idempotent replays still answered). mTLS, `CN=withdrawal-service` only. Naturally idempotent, no key. **No unfreeze endpoint** — operator clears it by manual SQL (invariant #19). |
| `POST /v1/sign/hot-wallet` | **PROPOSED HERE** — path is invented; the *contract* (fields + no-caller-chosen-key constraint) is decided; the gas-construction approach below is **Decided (Tech Lead, 2026-09-29)** — option (a): `wallet-service` owns construction (Open Questions #11) | `{"to_address","asset_id","amount","chain_id","idempotency_key","max_gas_price"?}`. **Decided** (Tech Lead review): `idempotency_key` is required — the same key the Withdrawal Request already carries throughout its lifecycle for that caller (`withdrawal-flow.md`, Ubiquitous language > *Withdrawal Request*), or the caller-specific PROPOSED key for the other two callers (see §3, §4). **Decided (Tech Lead, 2026-09-29):** no gas-parameter field for price/limit — `wallet-service` constructs the transaction itself (fetches gas price, estimates gas limit) via its own EVM RPC client, rather than accepting a caller-supplied gas price/limit. `max_gas_price` **is** accepted as an optional caller-supplied safety ceiling (never a dictated price) — same decision, both halves resolved together. | `{"signed_raw_tx","nonce"}` (hex tx, integer nonce). **Decided:** `nonce` is now part of the response — the caller needs it for its own state (e.g. `withdrawals.broadcast`'s payload already carries `nonce`, §1 above). On a **repeated call with an already-seen `idempotency_key`**, returns the *identical* stored `{signed_raw_tx, nonce}` instead of allocating/signing anything new — **Decided** (Tech Lead review, `withdrawal-flow.md` invariant #16): closes a lost-response gap where a retried call would otherwise claim a second nonce and strand the first, since EVM's strict per-account nonce ordering means the stranded nonce can never confirm. | Request shape + "no parameter that lets a caller pick a derivation index": `withdrawal-flow.md` invariant #5. Nonce allocation happens inside this call, atomically: invariant #6. Idempotency-per-key requirement: invariant #16 (new). Gas-construction decision and the `max_gas_price` option: this doc's Open Questions #11 (Decided), `withdrawal-flow.md` Open Questions #8 (Decided). Task table's shorthand path `POST /sign`: `docs/IMPLEMENTATION_PLAN.md` task 0.3 — not itself a design doc, cited for continuity only. |
| `POST /v1/sign/user-address` | **Decided: approved as proposed** (Open Questions #5, resolved) | `{"user_id","to_address","asset_id","amount","chain_id"}` | `{"signed_raw_tx"}` | Needed for the Sweep step: `deposit-flow.md` invariant #6 ("signed by the user's own derived key... `sweeper` asks for the signature, it never holds or derives the key itself") and its Sweep diagram (`Sweeper->>Wallet: sign(from=user address, to=Hot Wallet, ...)`). This is a **second, distinct signing capability** from the Hot Wallet one above, deliberately: the caller picks *which user key* signs (Sweep's whole job) but **never picks the destination** — `wallet-service` must enforce server-side that the destination is always the Hot Wallet, never trusting the caller's `to_address` for this endpoint. Now a standing invariant: `deposit-flow.md` invariant #7. **No idempotency key on this endpoint** — unchanged by this round of fixes; it stays idempotent-by-construction via `deposit-flow.md` invariants #8/#10 (`sweeper` always re-reads the live balance before signing), not by key, since a retried call naturally signs whatever's actually left rather than a stale amount. **The user address's own EVM nonce for this call is sourced by `wallet-service` via a live `eth_getTransactionCount(address, "pending")` RPC read at signing time — Decided (Tech Lead, 2026-09-30), `deposit-flow.md` invariant #12** — a different, simpler mechanism than the Hot Wallet's DB-allocated counter (invariant #6 above), justified there by the fact a user address never has two concurrent outbound signers the way the Hot Wallet does. |

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
not yet decided, since it touches already-shipped code. **`GET
/v1/addresses/{address}` (new, above): PROPOSED mTLS, `CN=chain-watcher`
(and `CN=withdrawal-service` — Decided, `withdrawal-flow.md` invariant #18)** — unlike the deposit-address endpoints, this one isn't deferred:
`chain-watcher` cannot resolve `user_id` at all without it (`deposit-
flow.md`'s Address Resolution), so it needs the CN check from the start,
same posture as `ledger-service`'s Lock/Settle/Release endpoints did
when *they* were introduced. This is also the first synchronous call
`chain-watcher` makes to any other service — `service-auth-mtls.md`'s
Authorization matrix and its "not in this matrix" note have both been
updated; `chain-watcher` now needs its own leaf certificate from task
0.6's tooling. **`GET /v1/wallet/hot-address` (new, above): PROPOSED
mTLS, `CN=sweeper`, `CN=withdrawal-service` or (added by the Gas Top-Up bug fix, `deposit-flow.md` invariant #17) `CN=chain-watcher`** — the known callers
(deposit-flow.md's Sweep, wallet-tiering.md's Treasury Sweep monitor),
same allow-list shape as `POST /v1/sign/hot-wallet` above since both
services already hold a leaf certificate for that endpoint.

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
| `GET /v1/deposit-address` | — (no `{userID}` path segment; `user_id` comes from `Authorization: Bearer <api_key>`, not the client) | Proxies `wallet-service`'s response verbatim. **Decided (Tech Lead review):** `api-gateway` calls `wallet-service`'s `POST /v1/users/{userID}/deposit-address` (§2 `wallet-service` table above), **not** the `GET` variant — `POST` is already idempotent get-or-create, so this always self-provisions a first-time user's address instead of ever surfacing a `404` to the client. | `docs/IMPLEMENTATION_PLAN.md` task 2.5 ("`GET /deposit-address` (proxies `wallet-service`)"); underlying responsibility from `docs/ARCHITECTURE.md`'s `api-gateway` row ("get deposit address"). `{userID}` dropped from the client-facing path, `user_id` no longer client-supplied: `identity-auth.md` invariants #3, #4. |
| `GET /v1/balances` | Optional `?asset_id=` query filter; no `user_id`/`{userID}` — resolved from `Authorization: Bearer <api_key>` | **Decided** (was PROPOSED HERE, low confidence — no longer, since `ledger.md`'s Total/Locked/Available split makes the shape a direct consequence of the schema, not a guess): `[{"asset_id","symbol","total_balance","locked_balance","available_balance"}]` — all three amounts as decimal strings (see "How to read the tables"), `available_balance = total_balance - locked_balance`, computed by `ledger-service` at read time, never stored. Returning only `balance` (the pre-Lock shape) is no longer sufficient: a user with a withdrawal in flight needs to see funds tied up separately from funds actually spendable. A user with no Account row yet for a given asset (never deposited) still gets a `0`/`0`/`0` row for every asset in the Asset Registry, not an empty/missing entry — see the new `ledger-service` endpoint below. | `docs/ARCHITECTURE.md` `api-gateway` row ("get balance"); `docs/IMPLEMENTATION_PLAN.md` task 2.5; `ledger.md` Ubiquitous language > *Total/Locked/Available Balance*. Implies a new synchronous `api-gateway → ledger-service` call — see §3 and the new `GET /v1/ledger/balances/{user_id}` row below (**new, Tech Lead review** — this proxy target didn't actually exist as a `ledger-service` endpoint before now). `{userID}` path segment and client-supplied `user_id` removed per `identity-auth.md` invariants #3, #4. |
| `GET /v1/history` | Optional pagination params; no `user_id`/`{userID}` — resolved from `Authorization: Bearer <api_key>` | **PROPOSED HERE, low confidence** — `history-service`'s own schema isn't designed (`docs/IMPLEMENTATION_PLAN.md`'s module map: "no dedicated design doc") | `docs/ARCHITECTURE.md` `api-gateway` row ("get... history"). `{userID}` path segment and client-supplied `user_id` removed per `identity-auth.md` invariants #3, #4. |
| `POST /v1/withdrawals` | `{"idempotency_key","asset_id","amount","to_address"}` — no `user_id` field; `api-gateway` injects the authenticated `user_id` before forwarding to `withdrawal-service` (§3) | New/first request — **Decided (Tech Lead), implemented in task 3.2:** passes validation → `202 {"idempotency_key","status":"validated"}` (becomes `"reserved"` once task 3.3 adds the Reservation); **any validation rejection → `422 {"idempotency_key","status":"rejected","reject_reason"}`** (never `202` + rejected), row terminal `rejected`, no Lock; no verdict (a validation dependency failed) → `503`, row left `requested` (key spent, retry with a new key). `reject_reason` codes, Decided: `asset_not_registered`, `invalid_to_address`, `amount_too_large` (above uint256), `below_minimum_withdrawal_amount`, `destination_not_whitelisted` (no entry, or entry still `pending`), `destination_on_screening_list`, `destination_is_system_wallet`, `destination_is_deposit_address`. "Returns quickly": the old text here was PROPOSED, but "returns quickly, doesn't block on confirmation" is decided (`withdrawal-flow.md` Alternatives: "Block the HTTP response until the withdrawal confirms — Rejected"). Duplicate request (same key, row already existed): **PROPOSED HERE** — `200` + "existing request's current status" (`withdrawal-flow.md` diagram says this in prose, doesn't give an HTTP status). **Same user + same key but a different `asset_id`, `amount` or `to_address`: `409`, nothing changed — Decided (Tech Lead), `withdrawal-flow.md` invariant #11.** Insufficient funds (task 3.3, from the ledger Reservation): `422` (`withdrawal-flow.md` diagram, verbatim) — **Decided (Tech Lead):** `422 {"idempotency_key","status":"rejected","reject_reason":"insufficient_funds"}`, same shape as validation rejections; row terminal `rejected`, no Lock exists. | Request shape: `withdrawal-flow.md` diagram, `Gateway->>WD: POST /withdrawals` arrow, minus `user_id` — that diagram predates `identity-auth.md`; `user_id` is dropped from the client-facing body per invariant #4 and instead server-injected before the `api-gateway → withdrawal-service` call (§3, unchanged shape there). **Validation order — Decided, as implemented (task 3.2), first failure wins:** (1) idempotent row creation first, invariant #11; (2a) asset registered — `asset_not_registered`; (2b) `to_address` well-formed and non-zero — `invalid_to_address`; (2c) `amount` ≤ uint256 max — `amount_too_large`; (2d) `amount` ≥ `min_withdrawal_amount` — `below_minimum_withdrawal_amount`, invariant #10; (3) `to_address` is an `active` Whitelist Entry — `destination_not_whitelisted`, invariant #12; (4) `to_address` not on the Screening List — `destination_on_screening_list`, invariant #14 / `compliance-screening.md` invariant #3; (5) `to_address` is not the Hot or Warm Wallet (local comparison) — `422 destination_is_system_wallet` (code **Decided**; Cold Wallet addresses are not rejected, Decided; Hot and Warm from `GET /v1/wallet/system-addresses`, Decided, `wallet-tiering.md` invariant #9); (6) `to_address` is not a deposit address (`GET /v1/addresses/{address}`, fail closed) — `422 destination_is_deposit_address` (Decided); `503` if that check cannot complete, and the key is then spent — retry with a new `idempotency_key` (`withdrawal-flow.md` invariants #11, #17, #18, Decided option B); (7) only then, the ledger Reservation, whose own overdraft check is `ledger.md` invariant #1. **Key scope (Decided, invariant #20):** `idempotency_key` is unique per authenticated user; a duplicate from the same user returns that user's row, never another user's. |
| `GET /v1/withdrawals/{idempotency_key}` | — (path param only — `idempotency_key`, not a user identifier, so it's unaffected by the `{userID}` removal elsewhere) | **Decided** (resolves Open Questions #6): `{"idempotency_key","status","tx_hash"}` — `status` is the Withdrawal Request's current state-machine value (`requested`\|`validated`\|`reserved`\|`signed`\|`broadcast`\|`confirmed`\|`rejected`\|`failed`\|`disputed`, per `withdrawal-flow.md`'s state list), `tx_hash` is `null` until known (set from the `broadcast` state onward); `404` if the key doesn't exist **or belongs to another user** (**Decided**, `withdrawal-flow.md` invariant #20: lookup is by `(authenticated user_id, idempotency_key)` — as written before, any user could read any withdrawal by key) | Needed because `POST /v1/withdrawals` returns quickly rather than blocking on confirmation (`withdrawal-flow.md` Alternatives: "Block the HTTP response until the withdrawal confirms — Rejected") — a client needs some way to check status afterward. State list: `withdrawal-flow.md`'s Withdrawal Request states paragraph and invariants #1-#9. |
| `POST /v1/whitelist` | `{"address"}` — no `user_id` field; `api-gateway` injects the authenticated `user_id` before forwarding | `{"id","user_id","address","status":"pending","added_at"}` (`user_id` still appears in the *response*, identifying whose entry this is — only the client-supplied request field is dropped) | Fields: `withdrawal-flow.md`'s Whitelist Entry lifecycle diagram (`Gateway->>WD: add whitelist entry (user_id, address)`) — `user_id` removed from the client-facing request per `identity-auth.md` invariant #4. Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. Cooldown behavior it must satisfy: invariant #13 (`pending` until Whitelist Cooldown elapses, computed on read). |
| `GET /v1/whitelist` (**Decided (Tech Lead)** — new) | — (no params; `user_id` from `Authorization: Bearer <api_key>`, never the client) | `200 [{"id","address","status","added_at"}]` — the caller's own entries only; `status` (`pending`\|`active`) computed on read (`now() >= added_at + 24h`), same as validation; ordered by `added_at` desc; empty list `[]` if none; no pagination for now | `api-gateway` proxies `withdrawal-service`'s `GET /v1/whitelist?user_id={injected}` over mTLS (`CN=api-gateway`, §3). Closes `withdrawal-flow.md` Open Questions #6's visibility gap: a user can see a `pending` entry an attacker added during the Whitelist Cooldown and remove it. Read-only, no idempotency key. |
| `DELETE /v1/whitelist/{id}` | — | `204`, idempotent (no-op if already gone) | Path: `docs/IMPLEMENTATION_PLAN.md` task 3.7. "Removal is instant... no state needed for that direction": `withdrawal-flow.md`, Ubiquitous language > *Whitelist Entry*, and invariant #13. |
| Internal Transfer endpoint (e.g. `POST /v1/transfers`) | — | — | **GAP.** Same reason as the `transfers.internal` topic above — `docs/ARCHITECTURE.md` names this as an `api-gateway` responsibility, but no design doc exists for it. Not proposing a shape. Whatever shape it eventually gets, it's still bound by the same rule as every other row in this table: `Authorization: Bearer <api_key>` required, no client-supplied `user_id` (`identity-auth.md` invariants #3, #4). |

### `ledger-service` (internal — not user-facing)

**Decided, resolving Gap #1/#2 raised on Tech Lead review of `ledger.md`'s
Total/Locked/Available rework:** the withdrawal Reservation no longer goes
through `POST /v1/ledger/transactions` (`apply`) at all — `apply()` now
has exactly two callers, deposits and corrections. Reserving, confirming,
and abandoning a withdrawal go through three new endpoints below instead,
mirroring `ledger.md`'s Lock/Settle/Release split one-to-one.

| Endpoint | Request | Response | Cites |
|---|---|---|---|
| `POST /v1/ledger/transactions` (**PROPOSED HERE** — path invented; **transport decided: REST**, resolving Open Questions #7). **Scope narrowed:** used only for deposits (`source_type="deposit"`) and corrections (`source_type="correction"`) — never for a withdrawal Reservation, which is `POST /v1/ledger/locks` below instead. | `{"idempotency_key","source_type":"deposit"\|"correction","entries":[{"account_ref":"user"\|"chain","user_id"?,"asset_id","amount","corrects_entry_id"?}]}`. **`corrects_entry_id` is required on every entry when `source_type="correction"`, and `amount` is ignored/recomputed server-side for those entries** — `ledger-service` derives the correction's actual amount as the exact negation of the referenced original entry, never trusting a caller-supplied correction amount (`ledger.md` invariant #11) — this is what stops the endpoint being usable to mint an arbitrary credit. | `{"status":"applied"\|"already_applied"}` or `422 {"status":"insufficient_funds"}` (a correction can only fail this way if it somehow double-spends a debit already reversed — should not happen given the idempotency key, but not structurally impossible to check for) or `400 {"status":"invalid_correction","reason":"..."}` if `source_type="correction"` and any entry is missing `corrects_entry_id` or references an entry that doesn't exist / was already corrected | Operation semantics and the exact accept/reject/no-op branches: `ledger.md`'s `apply()` sequence diagram (now scoped to deposit/correction only — Settle has its own endpoints below, never routed through this one, to avoid the double-decrement/non-atomicity bug an earlier draft had when Settle called into this same operation as a nested step). Sum-to-zero-per-asset enforcement: `ledger.md` invariant #2. Idempotent replay: invariant #3. Available-Balance-not-Total debit check: invariant #9. Correction integrity: invariant #11. Transport: **Decided — REST**, matching `wallet-service`'s already-implemented convention. **Auth: PROPOSED** — same mTLS + CN-authorization mechanism as `wallet-service`'s signing endpoints, restricted to `CN=withdrawal-service` only (corrections are the only caller-originated `source_type` this endpoint accepts; deposits are posted by `ledger-service`'s own Kafka consumer internally, not over this REST surface — see §1's `deposits.confirmed` row); see `docs/design/service-auth-mtls.md` invariant #7 and its Authorization matrix. Not yet built (ledger-service doesn't exist yet) — recommended to be built in from the start at Milestone 1 rather than retrofitted. |
| `POST /v1/ledger/locks` (**PROPOSED HERE** — new, resolves Gap #1/#2) | `{"idempotency_key","user_id","asset_id","amount"}` — **never `account_id`**: the caller (`withdrawal-service`) doesn't and shouldn't know `ledger-service`'s internal Account primary key; `ledger-service` resolves/creates the `(user_id, asset_id)` Account internally (`ledger.md`'s Lock definition) | `200 {"idempotency_key","status":"active","amount"}` (new lock, or an idempotent replay returning the existing lock's current status verbatim — could already be `"settled"`/`"released"` if this is a very late retry) or `422 {"status":"insufficient_funds"}` | `ledger.md`'s Lock → Settle / Release diagram and invariants #1, #6, #9, #10; `withdrawal-flow.md` invariant #1 (Reservation) |
| `POST /v1/ledger/locks/{idempotency_key}/settle` (**PROPOSED HERE** — new) | — (path param only; the amount and account were already fixed at Lock creation, nothing left for the caller to supply) | `200 {"idempotency_key","status":"settled","ledger_transaction_id"}` (newly settled, or idempotent no-op replay) or `404` (`idempotency_key` was never locked) | `ledger.md`'s Settle sub-diagram (single atomic transaction — lock-status guard, both balance updates, and both entry inserts together, fixing an earlier draft that split these across two transactions and could leave a lock `settled` with no Entry on a mid-flight crash) and invariant #7; `withdrawal-flow.md`'s `confirmed` transition |
| `POST /v1/ledger/locks/{idempotency_key}/release` (**PROPOSED HERE** — new) | — (path param only) | `200 {"idempotency_key","status":"released"}` (newly released, or idempotent no-op replay) or `404` | `ledger.md`'s Release sub-diagram and invariant #8; `withdrawal-flow.md`'s revised invariant #3 and new invariant #17 (Reaper) |
| `GET /v1/ledger/balances/{user_id}` (**PROPOSED HERE** — new, fixes a real gap: Tech Lead review found `api-gateway`'s `GET /v1/balances` had a decided response shape but no `ledger-service` endpoint to actually call for it — this doc had specified the client-facing contract without ever specifying the internal one behind it) | — (path param only; optional `?asset_id=` query filter, same as the `api-gateway` endpoint it backs) | `200 [{"asset_id","symbol","total_balance","locked_balance","available_balance"}]` — one row per Asset Registry entry, **always present even if the user has no Account row yet for that asset** (a never-deposited-into asset reads as `0`/`0`/`0`, computed by joining the Asset Registry against whatever Account rows do exist, left side always wins — never a `404` or a missing row, since "you've never touched this asset" and "your balance in it is zero" are the same fact from a balance-reader's point of view) | `ledger.md` Ubiquitous language > *Total/Locked/Available Balance*; `api-gateway`'s `GET /v1/balances` row above, which this endpoint exists solely to back |

---

## 3. Synchronous service-to-service calls

Per `docs/ARCHITECTURE.md` (now updated to match — see below): nearly
everything is event-driven, and the calls below are the full list of
exceptions. Most were already implied by a design doc before this
table existed (`ledger-service`'s `apply()`/Lock/Settle/Release, drawn
as direct calls in every sequence diagram that uses them); one is new
here — `chain-watcher`'s cache-miss fallback to `wallet-service`
(`deposit-flow.md`'s Address Resolution fix) — the first time
`chain-watcher` talks synchronously to anything.

**Auth column note:** "mTLS" below means "decided, per
`withdrawal-flow.md` Open Questions #2." Which CN is authorized for
each row, the CA behind it, and the enforcement mechanism inside the
receiving service are now specified in one place:
`docs/design/service-auth-mtls.md` — cited per-row below instead of
repeating the detail on every line.

| Caller | Callee | Call | Auth | Constraint | Cites |
|---|---|---|---|---|---|
| `withdrawal-service` | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` | mTLS — client cert `CN=withdrawal-service`, on this endpoint's allow-list (`service-auth-mtls.md` Authorization matrix) | Hot Wallet only; **no caller-chosen key** — the request has no field that could select a derivation index, capping blast radius at "can spend from the Hot Wallet" even if `withdrawal-service` is fully compromised. **`idempotency_key` = the Withdrawal Request's own `idempotency_key`** (unchanged, reused as-is — not minted fresh per call). **Decided (`withdrawal-flow.md` invariant #20): the Withdrawal Key `withdrawal:{withdrawal_id}` instead, never the client's string** (which is only unique per user and could collide with `gas-topup:...` keys). A repeated call with this key (e.g. after a lost response) gets back the identical `{signed_raw_tx, nonce}`, never a second nonce. | `withdrawal-flow.md` invariants #5, #16, diagram; CN-authorization enforcement: `service-auth-mtls.md` invariants #2, #4 |
| `sweeper` | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` — for **Gas Top-Up** (Hot Wallet → user address) | mTLS — client cert `CN=sweeper`, also on this endpoint's allow-list | Same Hot Wallet endpoint/constraint as above; only the destination varies. **`idempotency_key` = `gas-topup:{chain_id}:{tx_hash}:{log_index}` — Decided (Tech Lead, 2026-09-30)** (`deposit-flow.md` invariant #13), deterministically derived from the triggering deposit's own natural key (namespaced with a `gas-topup:` prefix so it can never collide with that deposit's ledger-side idempotency key). | `deposit-flow.md` Sweep diagram, invariant #13 (new); `withdrawal-flow.md` invariant #16; `service-auth-mtls.md` Authorization matrix |
| `sweeper` | `wallet-service` | `POST /v1/sign/user-address` — for the **Sweep** itself (user address → Hot Wallet) | mTLS — client cert `CN=sweeper` **only**; `withdrawal-service`'s certificate is explicitly **not** on this endpoint's allow-list (`service-auth-mtls.md` invariant #4) — this is what makes invariant #7's "no caller-chosen key" argument for this endpoint actually enforced, not just a request-shape convention | Signs as the specified user's derived key — this one *does* let the caller pick which key, by design (Sweep's whole job). **Decided (Open Questions #5, resolved): `wallet-service` enforces `to_address == Hot Wallet` server-side, never trusting the caller.** `to_address` itself is sourced by `sweeper` from `GET /v1/wallet/hot-address` (deposit-flow.md invariant #15), not a local constant. No idempotency key on this call — stays idempotent-by-construction (fresh balance read before signing, `deposit-flow.md` invariants #8/#10), unaffected by this round of fixes. **The user address's nonce for this call is sourced by `wallet-service` via a live `eth_getTransactionCount(address, "pending")` RPC read — Decided (Tech Lead, 2026-09-30)** (`deposit-flow.md` invariant #12), not a DB-tracked counter. | `deposit-flow.md` invariants #6, #7, #12, #15 (new); Sweep diagram; `service-auth-mtls.md` invariants #2, #4 |
| `withdrawal-service` (sweep-monitor loop) | `wallet-service` | `POST /v1/sign/hot-wallet` `{to_address, asset_id, amount, chain_id, idempotency_key}` → `{signed_raw_tx, nonce}` — for **Treasury Sweep** (Hot Wallet → Warm) | mTLS — same `CN=withdrawal-service` certificate as `withdrawal-service`'s other calls to this endpoint (it's the same service/process, not a separate identity) | Same Hot Wallet endpoint; destination is the Warm address instead of an external one. **`idempotency_key` = a fresh UUID minted once per Treasury Sweep Attempt row, reused across retries until that sweep broadcasts — PROPOSED** (`wallet-tiering.md` invariant #6), since a Treasury Sweep has no pre-existing client request to derive a key from the way a withdrawal or a gas top-up does. | `wallet-tiering.md` invariant #6 (new), diagram; `service-auth-mtls.md` Authorization matrix |
| `chain-watcher` | `wallet-service` | `GET /v1/addresses/{address}` → `{user_id, chain}` or `404` — **cache-miss fallback only**, the common path resolves via `chain-watcher`'s own Redis hash | **PROPOSED** — mTLS, client cert `CN=chain-watcher`, new leaf certificate (`chain-watcher` had none before this — `service-auth-mtls.md`'s Authorization matrix) | Read-only, no idempotency key needed. A `404` means the address genuinely isn't ours (no event emitted); a transport failure is retried, never treated as `404` — `deposit-flow.md`'s rewritten invariant #1, composes with invariant #4's watermark rule | `deposit-flow.md`'s new Address Resolution term and rewritten invariant #1; `service-auth-mtls.md` Authorization matrix (new row) |
| `sweeper` | `wallet-service` | `GET /v1/wallet/hot-address` → `{address}` — **called once at startup, result cached in memory**, not per-Sweep | **PROPOSED** — mTLS, client cert `CN=sweeper` (same certificate `sweeper` already holds for its two `/v1/sign/*` calls) | Read-only, no idempotency key needed. Replaces a locally-configured `to_address` constant that could drift from what `wallet-service` actually derived | `deposit-flow.md`'s new Hot Wallet Address Discovery term and invariant #15; `service-auth-mtls.md` Authorization matrix (new row) |
| `withdrawal-service` | `wallet-service` | `GET /v1/wallet/system-addresses` → `{chain_id, hot, warm}` — **once at startup, cached**; rejects withdrawals/whitelist entries to Hot or Warm (`422 destination_is_system_wallet`), and later gives the Treasury Sweep its Warm destination | **Decided (Tech Lead)** — mTLS, `CN=withdrawal-service` only | No withdrawal or whitelist request accepted until it succeeds; never an env var | `wallet-tiering.md` invariant #9; `withdrawal-flow.md` invariant #18 |
| `withdrawal-service` (Reaper, pipeline) | `wallet-service` | `GET /v1/sign/hot-wallet/{key}`, `POST /v1/sign/hot-wallet/{key}/void`, `POST /v1/sign/hot-wallet/{key}/replace` — its own Withdrawal Keys only | **PROPOSED** — mTLS, `CN=withdrawal-service` only; key scoped by `caller_cn` | Void: Reaper, `reserved` rows past the Stuck Reservation Timeout (Release only on `voided`). Replace: pipeline Fee Bump, `replacement_seq` = last + 1, at most 5 (PROPOSED). GET: ALERT detail | `withdrawal-flow.md` invariants #17, #21, #22; `service-auth-mtls.md` Authorization matrix |
| `withdrawal-service` | `wallet-service` | `POST /v1/wallet/nonce-gap` `{chain_id, from_nonce}` → `{chain_id, frozen_from_nonce, frozen_at}` — on detecting a Nonce Gap | **Decided (Tech Lead)** — mTLS, `CN=withdrawal-service` only (new endpoint) | Idempotent by `LEAST` semantics, retried until `200`; no unfreeze call exists | `withdrawal-flow.md` invariants #7, #8, #19; `service-auth-mtls.md` Authorization matrix |
| `withdrawal-service` | `wallet-service` | `GET /v1/wallet/hot-address` → `{address}` — **once at startup, cached**; also used to reject withdrawals/whitelist entries to the Hot Wallet | **Decided** (already on the allow-list for the sweep-monitor loop — same process, same certificate) | `withdrawal-service` accepts no withdrawal or whitelist request until it has the address | `withdrawal-flow.md` invariant #18 |
| `withdrawal-service` | `wallet-service` | `GET /v1/addresses/{address}` → `{user_id, chain}` or `404` — at Withdrawal Request validation and at Whitelist Entry creation, to refuse an address that is one of our own deposit addresses | **Decided (Tech Lead)** — mTLS, client cert `CN=withdrawal-service` (existing certificate), added to this endpoint's allow-list | Read-only, no idempotency key. `200` = ours -> reject (`422 destination_is_deposit_address`); `404` = not ours -> continue; any other outcome fails closed (`503`, never treated as `404`); no negative cache | `withdrawal-flow.md` invariant #18, Open Questions #9; `deposit-flow.md` Open Questions #13; `service-auth-mtls.md` Authorization matrix |
| `chain-watcher` | `wallet-service` | `GET /v1/wallet/hot-address` → `{address}` — **called once at startup, result cached in memory; `chain-watcher` does not start scanning until it succeeds** | **Decided (Tech Lead), implemented** — mTLS, client cert `CN=chain-watcher` (same certificate as its `GET /v1/addresses/{address}` calls), **newly added to this endpoint's allow-list** | Read-only, no idempotency key. Lets `chain-watcher` skip Hot Wallet Outflows (Gas Top-Up, withdrawals, Treasury Sweeps) instead of crediting them as deposits. Never an env var | `deposit-flow.md` invariant #17, Hot Wallet Address Discovery; `service-auth-mtls.md` Authorization matrix |
| `withdrawal-service` (sweep-monitor loop) | `wallet-service` | `GET /v1/wallet/hot-address` → `{address}` — **called once at startup, result cached in memory** | **PROPOSED** — mTLS, client cert `CN=withdrawal-service` (same certificate its other calls to `wallet-service` already use) | Read-only, no idempotency key needed. Same fix as the row above, for the Treasury Sweep monitor's own on-chain balance check | `wallet-tiering.md` invariant #8 (new); `service-auth-mtls.md` Authorization matrix (new row) |
| `withdrawal-service` | `ledger-service` | `POST /v1/ledger/locks` (`lock`) — for the Reservation at `reserved`, `.../settle` at `confirmed`, `.../release` on any pre-Settle failure or Reaper reclaim (`withdrawal-flow.md` invariant #17) | **Decided: REST** (Open Questions #7, resolved). **Auth: PROPOSED** — mTLS, client cert `CN=withdrawal-service` only, same mechanism as `wallet-service`'s endpoints, extended here for consistency (`service-auth-mtls.md` invariant #7) — not yet built, since `ledger-service` doesn't exist yet | Idempotent — see §4 | `ledger.md` Lock/Settle/Release diagram and invariants #1, #6–#10; `withdrawal-flow.md` invariants #1, #3, #17; `service-auth-mtls.md` invariant #7 |
| `withdrawal-service` | `ledger-service` | `POST /v1/ledger/transactions` (`apply`, `source_type="correction"`) — for a Compensating Entry (the `{idempotency_key}:refund` key — **Decided: `withdrawal:{withdrawal_id}:refund`**, `withdrawal-flow.md` invariant #20), only ever reached from `disputed` → abandoned (post-Settle case, `withdrawal-flow.md` invariant #9) | Same as the row above — **PROPOSED**, `CN=withdrawal-service` only | Idempotent — see §4. Correction integrity (must reference and exactly negate a real prior Entry): `ledger.md` invariant #11 | `ledger.md` `apply()` diagram and invariant #11; `withdrawal-flow.md`'s revised invariant #3 and Compensating Entry definition; `service-auth-mtls.md` invariant #7 |
| `api-gateway` | `wallet-service` | Proxies `GET/POST /v1/users/{userID}/deposit-address` | **PROPOSED** — mTLS, client cert `CN=api-gateway`, deferred (`service-auth-mtls.md` Open Questions #4): the general "who can be trusted to call this" question is already resolved at the *user-identity* level (the `userID`/`user_id` this call carries is always the one `api-gateway` resolved from the caller's `Authorization: Bearer <api_key>`, `identity-auth.md` invariants #3-#5), but at the *service-identity* level this hop still has no client-cert check today, unlike the `/v1/sign/*` endpoints | Pass-through | `docs/IMPLEMENTATION_PLAN.md` task 2.5; `service-auth-mtls.md` Authorization matrix and Open Questions #4 |
| `api-gateway` | `ledger-service` | `GET /v1/ledger/balances/{user_id}` — backs `GET /v1/balances` (§2), **new, Tech Lead review**: this row previously pointed at a `ledger-service` endpoint that didn't actually exist yet | **PROPOSED** — mTLS, `CN=api-gateway` | Pass-through, read-only | `ledger-service` §2 table above (new `GET /v1/ledger/balances/{user_id}` row); `service-auth-mtls.md` Authorization matrix |
| `api-gateway` | `withdrawal-service` | Proxies `POST /v1/withdrawals`, `GET /v1/withdrawals/{idempotency_key}`, `POST /v1/whitelist`, `GET /v1/whitelist` (as `GET /v1/whitelist?user_id={injected}`, Decided), `DELETE /v1/whitelist/{id}`, each with the injected `user_id` | **Decided (Tech Lead)** — mTLS, client cert `CN=api-gateway` (`service-auth-mtls.md` Open Questions #4, this hop only) | Pass-through with server-injected `user_id` | `service-auth-mtls.md` Authorization matrix |
| `api-gateway` | `ledger-service`, `history-service` | Proxies remaining history endpoints in §2 | **PROPOSED** — same as the row above: mTLS with `CN=api-gateway`, applicable once each specific downstream endpoint is actually designed (several are still GAP/PROPOSED HERE, §2) — `service-auth-mtls.md` deliberately doesn't invent those paths just to attach a CN rule to them yet | Pass-through | `docs/ARCHITECTURE.md` `api-gateway` row: "Talks to: all of the above"; `service-auth-mtls.md` Authorization matrix |

---

## 4. Idempotency keys — consolidated

| Topic / Endpoint | Idempotency key | Enforced by |
|---|---|---|
| `deposits.detected` | `(chain_id, tx_hash, log_index)` | `observed_transfers` unique constraint, `deposit-flow.md` invariant #3 |
| `deposits.confirmed` | `(chain_id, tx_hash, log_index)` | Same, plus `ledger_transactions` `UNIQUE(chain_id, tx_hash, log_index)`, `ledger.md` invariant #3 |
| ~~`withdrawals.requested`~~ | — | **Decided against, not built** — see §1's "considered, decided against" note (resolves Open Questions #4) |
| `withdrawals.broadcast` | `idempotency_key` (**Decided: `withdrawal_id`**, `withdrawal-flow.md` invariant #20 — the client key is only unique per user) | Same; also naturally becomes unique on `(chain_id, tx_hash)` once broadcast, since invariant #2 guarantees at-most-once signing |
| `withdrawals.confirmed` | `idempotency_key` (**Decided: `withdrawal_id`**, as above) | Same |
| `withdrawals.failed` | `withdrawal_id` (**PROPOSED** payload; topic Decided) | A request reaches `failed` once (guarded UPDATE); outbox publish at-least-once; `history-service` upserts on `withdrawal_id` |
| `transfers.internal` | **GAP** — not designed | — |
| `POST /v1/withdrawals` | `idempotency_key`, client-supplied in the request body — **Decided: unique per user, never used downstream** (`withdrawal-flow.md` invariant #20). Same key with a different `asset_id`/`amount`/`to_address` → `409` (**Decided**, invariant #11) | `UNIQUE(idempotency_key)` (**Decided: `UNIQUE(user_id, idempotency_key)`**, plus a server-minted `withdrawal_id` PK) on `withdrawal_requests`, `INSERT ... ON CONFLICT DO NOTHING`, `withdrawal-flow.md` invariant #11 |
| `POST /v1/sign/hot-wallet` | **Decided (Tech Lead review): required, per-caller.** `wallet-service` persists `(idempotency_key -> nonce, signed_raw_tx)`; a repeated call with the same key returns the identical stored result instead of allocating a new nonce — closes a lost-response gap invariant #2/#6 alone didn't cover (a lost response followed by a retry would otherwise strand a nonce forever, the same failure shape as a Nonce Gap but from a different cause). Three distinct callers, three key sources (**PROPOSED:** each signature row also records the caller's CN, `caller_cn`, so lookup/void/replace only act on the caller's own keys — `withdrawal-flow.md` invariant #22; a voided key returns `410`): (1) `withdrawal-service` → the Withdrawal Request's own `idempotency_key`, unchanged/reused (`withdrawal-flow.md`, Ubiquitous language) — **Decided: the Withdrawal Key `withdrawal:{withdrawal_id}`** (invariant #20); (2) `sweeper` (Gas Top-Up) → **Decided** `gas-topup:{chain_id}:{tx_hash}:{log_index}`, derived from the triggering deposit's natural key (`deposit-flow.md` invariant #13); (3) `withdrawal-service` sweep-monitor (Treasury Sweep) → **PROPOSED** a fresh UUID minted once per Treasury Sweep Attempt row and reused across retries (`wallet-tiering.md` invariant #6). | `withdrawal-flow.md` invariant #16 (new); `deposit-flow.md` invariant #13 (new, Decided); `wallet-tiering.md` invariant #6 (new, PROPOSED) |
| `POST /v1/sign/user-address` (Sweep) | **None; idempotent by construction instead of by key** — `sweeper` always re-reads the address's live balance immediately before signing (invariant #10), so a retried call signs whatever's actually left (near-zero after a successful earlier attempt), never a stale amount. Unchanged by this round of fixes — Gap 1's idempotency-key requirement was scoped to `/v1/sign/hot-wallet`'s three callers only, not this endpoint. | `deposit-flow.md` invariants #8, #10 |
| `POST /v1/ledger/locks` / `.../settle` / `.../release` | `idempotency_key` — same key as the Withdrawal Request throughout (**Decided: the Withdrawal Key `withdrawal:{withdrawal_id}`**, `withdrawal-flow.md` invariant #20), `lock()`/`settle()`/`release()` all keyed on it (`ledger.md`'s Lock is keyed 1:1 with the Withdrawal Request, never a derived key) | `locks.idempotency_key` unique, `ON CONFLICT DO NOTHING` on creation; `status` transitions guarded (`UPDATE ... WHERE status='active'`), `ledger.md` invariants #6, #7 |
| `POST /v1/ledger/transactions` (`apply`) | `idempotency_key` (deposit: `(chain_id, tx_hash, log_index)`; correction/refund: `{idempotency_key}:refund` (**Decided: `withdrawal:{withdrawal_id}:refund`**), derived from the original Withdrawal Request's key — **the only `source_type` `withdrawal-service` may post here now**, since Reservation moved to `POST /v1/ledger/locks` above) | `ledger_transactions.idempotency_key` unique, `ON CONFLICT DO NOTHING`, `ledger.md` invariant #3; refund key derivation: `withdrawal-flow.md`, Ubiquitous language > *Compensating Entry*; correction-integrity check (not just idempotency): `ledger.md` invariant #11 |
| `POST /v1/whitelist` | (**Decided**, `withdrawal-flow.md` invariant #18: refuses a deposit address — `422 destination_is_deposit_address` — and the Hot/Warm Wallet — `422 destination_is_system_wallet`, Decided; Cold not rejected; `503` if the check cannot be completed.) **Decided:** `UNIQUE(user_id, address)` — re-adding an already-`pending`/`active` entry no-ops rather than erroring or restarting its cooldown (Open Questions #10, resolved) | `whitelist_entries` unique constraint; now also `withdrawal-flow.md` invariant #15 |
| `GET /v1/wallet/system-addresses` | **None needed — read-only**, same answer for a running instance | `wallet-tiering.md` invariant #9 |
| `POST /v1/sign/hot-wallet/{key}/replace` | `(key, replacement_seq)` — **PROPOSED**; same seq replays the stored replacement, a gap is `409` | `hot_wallet_replacements` PK; `withdrawal-flow.md` invariant #21 |
| `POST /v1/sign/hot-wallet/{key}/void` | **None needed** — idempotent by construction (tombstone or existing signature; repeat returns the same answer) | `withdrawal-flow.md` invariant #22 |
| `POST /v1/wallet/nonce-gap` | **None needed** (Decided endpoint) — `LEAST(existing, from_nonce)` makes a repeat a no-op and a lower value only widens the freeze | `withdrawal-flow.md` invariant #19 |
| `GET /v1/whitelist` | **None needed — read-only** | `withdrawal-flow.md` Whitelist Entry term |
| `DELETE /v1/whitelist/{id}` | Naturally idempotent — deleting a non-existent id is a no-op, not an error | — |
| `GET /v1/addresses/{address}` | **None needed — read-only.** A repeated call with the same address always returns the same answer (the mapping is immutable once an address is issued, per `wallet-service`'s existing `CreateAddress`), so there's no mutation to guard against replaying | `deposit-flow.md`'s Address Resolution term |
| `GET /v1/wallet/hot-address` | **None needed — read-only.** Same answer every time for a running instance | `deposit-flow.md`'s Hot Wallet Address Discovery term |
| `GET /v1/ledger/balances/{user_id}` | **None needed — read-only.** No mutation to guard against replaying | `ledger-service` §2 table above (new row) |

---

## 5. Consumer failure handling (no dead-letter queue)

**New, Tech Lead review.** No design doc previously said what a Kafka
consumer does when it keeps failing to process a message — a real gap:
"retry forever" and "give up after N tries" are very different
behaviors with very different correctness consequences depending on
*which* consumer it is, so a single blanket answer would be wrong for
at least one of them. Two policies, chosen per consumer by whether
giving up ever loses something correctness depends on:

| Consumer | Topic | Policy | Why |
|---|---|---|---|
| `ledger-service` | `deposits.confirmed` | **Retry indefinitely — never give up, never skip.** No bounded retry count, no dead-letter topic; the consumer offset only advances once `apply()`/the credit actually succeeds. | A deposit that's never applied is a permanently uncredited user balance — the one failure mode this project cannot tolerate silently. A stuck message here should page an operator (Postgres down, etc.), not get quietly skipped. |
| `sweeper` | `deposits.confirmed` | **Bounded retry (10 attempts), then `ALERT`-level log + skip** (`deposit-flow.md` invariant #16). Offset still advances — `sweeper` moves on. | Sweep is custody housekeeping, not a balance-affecting operation (`deposit-flow.md` invariant #9) — the user's funds are already correctly credited and sitting safely at their own attribution address regardless of whether the physical sweep ever completes. An operator can re-drive the skipped record by hand once the underlying cause is fixed; nothing is lost in the meantime. |
| `history-service` | all four topics it consumes (§1) | **Same as `sweeper`: bounded retry (10 attempts), then `ALERT`-level log + skip.** Offset still advances. | `history-service` is a pure read-model (`docs/IMPLEMENTATION_PLAN.md`'s module map) — no invariant anywhere depends on it. A row that never lands there costs a user one missing line in their history view, never an incorrect balance or a lost fund. |

No topic in this project gets an actual dead-letter *topic* — "skip and
`ALERT`-log" plays that role for the two consumers where skipping is
safe, and isn't offered at all to the one consumer where it wouldn't be
(`ledger-service`). A true dead-letter topic (redirecting the failed
message somewhere replayable instead of just logging its key) is a
reasonable later addition if `ALERT`-log-and-skip turns out to lose
operational visibility in practice — not designed now since nothing
today needs it beyond what a searchable `ALERT` log line already gives.

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
   **Update, Tech Lead review:** the *storage* side of this flagged risk
   turned out to be real, not just theoretical — `ledger.md`'s schema
   was still `bigint` (int64, overflows at ~9.2 ETH in wei) when this
   question was first raised. Fixed: every amount/balance column in
   `ledger.md` is now `NUMERIC(78,0)` (`ledger.md` invariant #10). The
   wire format here was never the problem (decimal string never
   overflows); the schema was.
10. ~~**`POST /v1/whitelist`'s idempotency key is proposed, not decided**
    (`UNIQUE(user_id, address)`, §4 above) — confirm re-adding an
    existing entry should no-op rather than error or restart its
    cooldown.~~
    **Decided: `UNIQUE(user_id, address)`.** Re-adding an existing
    pending/active entry no-ops rather than erroring or restarting its
    cooldown. Now also stated in `withdrawal-flow.md` as invariant #15.
11. **Decided (Tech Lead): (a) + `max_gas_price` — `wallet-service` owns full
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
    **Decision (Tech Lead, 2026-09-29): option (a), and yes to the optional
    `max_gas_price` ceiling.** The remaining `PROPOSED` markers on
    `/v1/sign/hot-wallet` for gas construction and `max_gas_price` in §2 are
    now settled; the SA can drop them on the next pass.
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
13. **New, from Tech Lead review of `ledger.md`'s Total/Locked/Available
    rework: the wire contract for `lock`/`settle`/`release` didn't exist
    at all** — the rework added three new operations to `ledger.md`'s
    design but this doc, written before that rework, still only listed
    `POST /v1/ledger/transactions`. **Resolved here:** three new
    `ledger-service` endpoints added to §2
    (`POST /v1/ledger/locks`, `.../settle`, `.../release`), `apply()`'s
    row scope-narrowed to deposit/correction only, §3's sync-call row
    split in two (Lock/Settle/Release vs. the now-narrower `apply()`),
    and §4's idempotency table updated to match. Same review also
    caught: (a) `lock()`'s proposed signature took `account_id`, which
    `withdrawal-service` structurally can't supply (`ledger-service`'s
    own internal primary key) — fixed to `(user_id, asset_id)`,
    resolved server-side; (b) `GET /v1/balances`'s response was still
    the pre-rework single-`balance` shape — fixed to
    `{total_balance, locked_balance, available_balance}`; (c) `ledger.md`
    still specified `bigint` storage, which overflows at ~9.2 ETH in wei
    — fixed to `NUMERIC(78,0)` (`ledger.md` invariant #10); (d) nothing
    stopped a correction from crediting an arbitrary amount to an
    arbitrary account — fixed by requiring `corrects_entry_id` and
    server-computed exact negation (`ledger.md` invariant #11); (e) the
    Settle diagram had a real bug — it decremented `total_balance`
    directly *and* called the generic `apply()` (which also decrements
    `total_balance` for its debit entry) as a nested step, double-
    counting the debit, and split the lock's status flip into a separate
    transaction from the balance/entry writes, so a mid-flight crash
    could leave a Lock `settled` with no Entry — fixed by making Settle
    one atomic operation, never routed through `apply()` (`ledger.md`'s
    Diagram section, `POST /v1/ledger/locks/{key}/settle` above); (f)
    nothing reclaimed a Lock that never resolved (crash before
    broadcast, or a tx that's genuinely stuck) — fixed by
    `withdrawal-flow.md`'s new Reaper invariant #17, using the same
    nonce-comparison proof technique the Nonce Gap design already
    established rather than a bare timeout. All of (a)-(f) were real
    correctness/contract gaps, not judgment calls, so fixed directly
    rather than left open — see `ledger.md` and `withdrawal-flow.md` for
    the invariant-level detail cited throughout.
14. **New, from Tech Lead review: neither `deposits.detected` nor
    `deposits.confirmed` carried a `user_id`, and only `wallet-service`
    knows the address→user mapping.** `ledger-service`'s Account is
    keyed by `(user_id, asset)`, never by address, so as drafted there
    was no way for `ledger-service` to know whose Account to credit —
    the deposit flow could not actually credit anyone. **Fixed:** both
    §1 payloads above gain a `user_id` field, resolved by `chain-watcher`
    (`deposit-flow.md`'s new Address Resolution term) via a Redis hash
    it now maintains plus a new `wallet-service` endpoint,
    `GET /v1/addresses/{address}` (§2), for the cache-miss path — never
    a direct cross-service Postgres read, which `deposit-flow.md`'s
    earlier phrasing had loosely implied. This makes `chain-watcher` a
    synchronous `wallet-service` caller for the first time (§3, new
    row) — `service-auth-mtls.md`'s Authorization matrix and its "not in
    this matrix" note (which previously said `chain-watcher` needs no
    leaf certificate at all) are both updated to match.
15. **New, from Tech Lead review (2026-09-30), while implementing
    `sweeper`/`api-gateway`: five more real gaps, all fixed directly.**
    (a) Open Questions #7/#8 above approved as proposed — `sweeper`'s
    actual implementation already matches. (b) `GET /v1/balances` had a
    decided client-facing response shape but no `ledger-service` endpoint
    behind it — fixed: new `GET /v1/ledger/balances/{user_id}` (§2), a
    real gap this doc should have caught the first time it specified the
    client-facing contract without the internal one behind it. (c) No
    decision existed for how `api-gateway` handles a first-time user's
    deposit-address lookup (the plain `GET` 404s) — fixed: `api-gateway`
    calls `wallet-service`'s `POST` (already idempotent get-or-create)
    instead of `GET`, never a separate create-on-404 round trip. (d) No
    design existed for how `sweeper`/`withdrawal-service`'s Treasury
    Sweep monitor learn the Hot Wallet's own address — fixed: new
    `GET /v1/wallet/hot-address` (§2), replacing a locally-configured
    constant that could drift. (e) No documented Kafka consumer
    failure/dead-letter policy existed anywhere — fixed: new §5, three
    consumers, two policies (retry-forever for `ledger-service`; bounded
    retry + `ALERT`-log + skip for `sweeper`/`history-service`), chosen
    by whether giving up on a message ever loses something correctness
    depends on. See `deposit-flow.md` and `wallet-tiering.md` for the
    invariant-level detail behind (a), (c), (d).
