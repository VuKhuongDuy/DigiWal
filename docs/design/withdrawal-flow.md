# Withdrawal flow

## Problem

A user with a positive ledger balance (`ledger.md`) requests that funds
be sent to an external on-chain address. Per your decision, this is
fully automatic — no manual review queue. The system must validate the
request, reserve the funds against the ledger, get a signature from
`wallet-service` (new: a signing endpoint, per your decision), broadcast
the transaction, and track it to confirmation — while never leaving a
user debited for a withdrawal that didn't actually happen, and never
signing the same withdrawal twice.

## Bounded context(s)

New bounded context: **Withdrawal**, owned by `withdrawal-service`
(named in `docs/ARCHITECTURE.md`). Talks to `ledger-service` (reserve
funds), `wallet-service` (sign — new surface), the EVM testnet RPC
(broadcast + confirmation), Kafka (producer), and its own Postgres
(withdrawal request state machine).

This also **extends the Wallet & Signing context**: `wallet-service`
gains a signing endpoint. That's a real widening of its trust boundary
— today it only ever returns public addresses; after this it can move
funds on request — so the invariants below treat it as seriously as the
existing key-custody design does.

**Depends on `identity-auth.md`**: every `user_id` referenced below is
the authenticated caller's, resolved by `api-gateway` from an API Key —
never a client-supplied field. This doc assumed that dependency existed
before `identity-auth.md` actually specified it (see former Open
Question #7).

## Ubiquitous language

- **Withdrawal Request** — one user ask to send `amount` of `asset` to
  `to_address`, identified by a client-supplied `idempotency_key`
  (minted by `api-gateway`, reused as the Ledger Transaction's
  idempotency key too — one key, one meaning, across both contexts).
  `UNIQUE(idempotency_key)` on the `withdrawal_requests` table — the
  request's own creation is idempotent, not just its later steps (see
  invariant #11). **This same `idempotency_key` is also reused, unchanged,
  as the idempotency key on the `POST /v1/sign/hot-wallet` call this
  request eventually makes** — see invariant #16.
  **Revised — Decided (Tech Lead), invariant #20, Open Questions #11:** the key is client-supplied (the client sends it in the
  `POST /v1/withdrawals` body; `api-gateway` forwards it, it does not mint
  it — the "minted by `api-gateway`" wording above contradicts
  `integration-contracts.md` §2/§4 and is superseded). It is unique **per
  user**, `UNIQUE(user_id, idempotency_key)`, and it is **never** used
  outside `withdrawal-service`: every downstream key is the Withdrawal
  Key below.
- **Withdrawal Key** — **Decided (Tech Lead), new (invariant #20).**
  `withdrawal:{withdrawal_id}`, where `withdrawal_id` is a UUID
  `withdrawal-service` mints when it inserts the Withdrawal Request row.
  Globally unique by construction, never chosen by a client. It is the
  key used for the ledger Lock/Settle/Release, for `POST
  /v1/sign/hot-wallet`, and (with a `:refund` suffix) for a
  Compensating Entry; `withdrawal_id` is carried on
  `withdrawals.broadcast`/`withdrawals.confirmed`.
- **Hot Wallet** — the single on-chain address (per this doc's
  recommendation — see Open Questions #1) that every withdrawal is
  actually paid out from, regardless of which address the user
  originally deposited to. Derived from its own, separate seed that
  `wallet-service` holds apart from the seed that derives user deposit
  addresses — **not** a reserved index in the same HD tree as user
  addresses. (This entry previously said the opposite — "a reserved
  derivation index in `wallet-service`'s existing HD wallet, distinct
  from user addresses" — written before `wallet-tiering.md` existed.
  That was stale, not a live disagreement: `wallet-tiering.md` invariant
  #1 later decided, and the Tech Lead approved, that Hot's signing key
  must be a separate seed precisely so one compromised seed can never
  expose both "every user's transient private key" and "the automated
  payout key." Fixed here to match. Source of truth going forward:
  `wallet-tiering.md` invariant #1, not this doc.)
- **Reservation** — the Lock (`ledger.md`) placed against the user's
  Account the moment a Withdrawal Request is accepted (not when it
  finally confirms on-chain). Reserves capacity out of the user's
  Available Balance without moving any real money yet: no Entry is
  posted, `total_balance` doesn't change, only `locked_balance` does.
  Uses `ledger.md`'s Lock-creation invariant (Available Balance can
  never go negative) as its safety net. Resolves to a real debit only
  when the Withdrawal Request actually reaches `confirmed` (Settle,
  `ledger.md`) — everything before that is reversible by Release alone,
  no Compensating Entry needed (see invariant #3, revised).
- **Hot Wallet Nonce** — the EVM account nonce for the Hot Wallet
  address. Must be allocated atomically and monotonically, exactly like
  `wallet-service`'s existing per-user derivation-index allocation — the
  same class of bug (two concurrent signs claiming the same value)
  applies here too. (This is a distinct nonce sequence from a user
  address's own EVM nonce, used only for Sweep transactions — see
  `deposit-flow.md`'s Sweep-related invariants for that, unrelated,
  sequence.)
- **Compensating Entry** — a new Ledger Transaction that credits the
  user back after a Withdrawal Request that had already **Settled**
  (`ledger.md` — real money already left `total_balance`) turns out not
  to have actually landed on the canonical chain. **This is now the
  only case that needs one**, since Settle is the sole point where an
  Entry is posted: it's the `disputed` → abandoned path after a Nonce
  Gap (below), reached only from `confirmed` (invariant #9). Any
  Withdrawal Request that fails *before* Settle — i.e., before
  `confirmed` — never had an Entry posted for it in the first place, so
  it's reversed by simply Releasing its Lock (`ledger.md`), not by a
  Compensating Entry (see invariant #3, revised). Never a mutation of
  the original debit (`ledger.md`'s immutability invariant). Uses its
  own idempotency key, deterministically derived from the original —
  **`{idempotency_key}:refund`** — never the original key itself
  (that's already taken by the Settle-time debit, and
  `ledger_transactions.idempotency_key` is unique). A retried
  compensation attempt hits the same `{idempotency_key}:refund` key
  (**Decided, invariant #20: `{Withdrawal Key}:refund`, i.e.
  `withdrawal:{withdrawal_id}:refund`**) and
  no-ops via `ledger.md`'s own idempotent-apply mechanism, so a crash
  between posting the refund and marking the Withdrawal Request
  `failed` can never double-refund.
- **Nonce Gap** — the situation where a Hot Wallet transaction at nonce
  N is no longer present in the canonical chain (a reorg deeper than
  Withdrawal Confirmation Depth). By EVM's strict per-account nonce
  ordering, this invalidates every Hot Wallet transaction at nonce ≥ N
  on the new canonical chain too — not just the one at N. Both
  `withdrawal-service` and `sweeper` draw nonces from the same Hot
  Wallet sequence, so a Nonce Gap can freeze both withdrawals and
  Sweeps/Gas Top-Ups (`deposit-flow.md`) at once. **A second, distinct
  cause of the same failure shape (a stranded, never-confirmable nonce)
  is now also prevented separately: see invariant #16** — that one is
  caused by a lost HTTP response, not a reorg, and is prevented at
  allocation time rather than detected after the fact.
- **Withdrawal Confirmation Depth** — a deeper confirmation threshold
  than deposits use (`deposit-flow.md`'s Confirmation Depth) before
  marking a Withdrawal Request `confirmed`, since funds leaving custody
  are more sensitive than funds arriving. Reduces how often a Nonce Gap
  happens; can't eliminate it. **Decided: 64 blocks (~12.8 minutes at
  Sepolia's ~12s average block time)** — exactly double
  `deposit-flow.md`'s 32-block Confirmation Depth, per this term's own
  stated invariant. Also grounded in a real Ethereum post-merge concept,
  same as the deposit-side number: 64 blocks ≈ two epochs, roughly what
  a full node exposes via the `finalized` block tag (reverting a
  finalized block requires a large-scale validator-slashing event, not
  an ordinary short reorg) — a meaningfully stronger guarantee than
  deposit-flow.md's `safe`-tag-equivalent depth, appropriate since these
  are funds leaving custody rather than arriving.
- **Disputed** — the state a `confirmed` Withdrawal Request moves to
  when a Nonce Gap is detected at or above its nonce. Resolved only by
  explicit operator action — back to `confirmed` (original tx safely
  rebroadcast at the same nonce) or to `failed` with a Compensating
  Entry (abandoned, after a nonce-filler tx clears the gap).
- **Minimum Withdrawal Amount** — the smallest amount of an asset a
  Withdrawal Request may move, set per-asset on the Asset Registry
  (`ledger.md`'s `min_withdrawal_amount` field) at registration time.
  **Decided:** for native coin, 15× the current typical gas cost of a
  withdrawal tx (e.g. ~21,000 gas × 2 gwei ≈ 0.000042 ETH gas cost →
  min ≈ 0.0006 ETH); for ERC-20, set manually by the operator at
  registration, since there's no price oracle to compare a token's
  value against gas cost automatically (`ledger.md`'s Open Questions
  #3). Exists both for economics (a withdrawal shouldn't cost the Hot
  Wallet more in gas than it moves) and to make spamming
  `withdrawal-service` with dust requests — and burning through Hot
  Wallet nonces doing it — less attractive.
- **Withdrawal Whitelist** — the set of addresses a user has approved
  as withdrawal destinations. A Withdrawal Request's `to_address` must
  be an `active` entry in the requester's whitelist, checked at
  validation alongside Minimum Withdrawal Amount.
- **Whitelist Entry** — one `(user_id, address)` pair, state `pending`
  (just added) or `active` (usable for withdrawal). `(user_id, address)`
  is unique — see invariant #15. Removal is instant — an entry simply
  stops existing, no state needed for that direction.
- **Whitelist Cooldown** — **Decided: 24 hours.** The delay between a
  Whitelist Entry's `added_at` and the moment it becomes `active`. This
  is the actual security property, not the whitelist's existence: an
  attacker who compromises a user's session can add their own address,
  but can't withdraw to it for 24h — giving the real user a window to
  notice and remove it. Computed on read (`now() >= added_at +
  cooldown`), not by a background job flipping status — a missed or
  delayed job would otherwise silently widen the window.

Withdrawal Request states: a row exists first (invariant #11), then
moves `requested → validated → reserved → signed → broadcast →
confirmed`, with `confirmed` the only transition that Settles the Lock
(`ledger.md`) and posts the real debit Entry — everything before it is
still just a Lock, no money has actually moved. `rejected` (failed
validation or insufficient available funds, terminal, no Lock ever
created) and `failed` (terminal) are the off-ramps: reached from
`reserved`/`signed`/`broadcast` it means Releasing the Lock (no Entry
was ever posted, so nothing needs compensating); reached from
`disputed` (below) it means a Compensating Entry, since by then the
Lock had already Settled and real money had already moved — see the
revised invariant #3. `disputed` (a `confirmed` request whose tx was
reorged out — see invariants #7–9) resolves back to `confirmed` or
forward to `failed`, always by explicit operator action. See Open
Questions #3 for the one remaining unresolved failure mode.

## Aggregates & invariants

1. **A Withdrawal Request's ledger Reservation and its own status move
   together.** `withdrawal-service` calls `ledger.lock(idempotency_key,
   user_id, asset_id, amount)` — **not `account_id`**: `withdrawal-service`
   never knows `ledger-service`'s internal Account primary key, only the
   `user_id` it already has (from the authenticated request) and the
   `asset_id` the withdrawal names; `ledger-service` resolves the Account
   internally (`ledger.md`'s Lock definition). Only on success does
   `withdrawal-service` set status = `reserved`. On "insufficient
   available funds," it sets `rejected` — never a partial lock.
2. **A Withdrawal Request can be signed at most once.** Guarded the same
   way as ledger idempotency: `UPDATE withdrawal_requests SET
   status='signing' WHERE status='reserved'`, zero rows affected means
   a duplicate/retry, skip. Prevents a retried event from asking
   `wallet-service` to sign (and therefore spend) the same withdrawal
   twice.
3. **A Withdrawal Request that fails before it Settles is reversed by
   Releasing its Lock, never by a Compensating Entry.** Since the
   Reservation (`ledger.md`'s Lock) never posts an Entry, a failure
   anywhere between `reserved` and `broadcast` — a signing failure, a
   broadcast failure, or any abandonment short of `confirmed` — is
   undone with `ledger.release(idempotency_key)`: `locked_balance` goes
   back down, `total_balance` was never touched, so there's nothing to
   compensate. This replaces this doc's earlier design, where the
   Reservation debited immediately and every such failure needed a
   refund Entry. A Compensating Entry is now needed only for the one
   case where real money already moved: a `confirmed` (already-Settled)
   Withdrawal Request later reorged out and abandoned (`disputed` →
   `failed`, invariant #9) — see the revised Compensating Entry
   definition above.
4. **A Withdrawal Request that has already been broadcast is never
   auto-released.** Only a request that provably never reached the chain
   is safe to reverse automatically — releasing a Lock for a broadcast
   tx that later lands on-chain would let a Settle attempt run against
   capacity that's already been given back to the user. Anything with a
   tx hash needs positive proof it didn't land before any Lock
   resolution — see Open Questions #3.
5. **`wallet-service` signs for the Hot Wallet only, and the caller
   specifies *what* to send, never *which key* nor *how the transaction
   is built*.** The signing request carries `{to_address, asset_id,
   amount, chain_id, idempotency_key}` (the `idempotency_key` addition
   is new — see invariant #16) — there is no parameter that lets a
   caller pick a derivation index, and (**PROPOSED**, Tech Lead sign-off
   pending — full design in `docs/design/integration-contracts.md` §2)
   no gas parameter either: `wallet-service` is proposed to construct
   the transaction itself, fetching current gas price and estimating gas
   limit via its own EVM RPC client, rather than trusting a caller-
   supplied gas price/limit. Either way, the caller can never dictate a
   derivation index; the gas-construction question is the one part of
   this invariant not yet decided. This means even a fully compromised
   `withdrawal-service` can't turn the signing endpoint into a way to
   drain an arbitrary user's address; the blast radius is capped at "can
   spend from the Hot Wallet" (and, if the gas-construction proposal is
   approved, "at a price it doesn't get to pick either"). An optional
   caller-supplied `max_gas_price` safety ceiling (never a dictated
   price, just a cap on cost exposure) is also **PROPOSED** — see
   `integration-contracts.md` §2 and this doc's Open Questions.
   **Cross-reference:** this "blast radius capped" argument depends on
   `wallet-service` actually enforcing *who* is allowed to call this
   endpoint by client-certificate identity, not just that the request
   body has no key-selecting field — see
   `docs/design/service-auth-mtls.md` invariants #2 and #4
   (CN-based authorization).
6. **Hot Wallet nonce allocation is atomic and monotonic** — same
   `UPDATE ... RETURNING` pattern `wallet-service` already uses for
   derivation-index allocation (`internal/storage/postgres.go`'s
   `allocateIndex`). No two signing calls can be issued the same nonce.
   This prevents two *different* signing calls from colliding on one
   nonce; it does not, by itself, prevent one caller's *retried* call
   from being allocated a *different* nonce for what should have been
   the same request — see invariant #16, which closes that gap.
7. **`wallet-service` refuses to allocate a new Hot Wallet nonce while a
   Nonce Gap is open, regardless of who's asking.** `withdrawal-service`
   and `sweeper` both draw from the same Hot Wallet nonce sequence, so
   this has to be enforced centrally, in the one place that actually
   allocates nonces — not duplicated (and possibly missed) in every
   caller.
8. **A detected Nonce Gap at N freezes every Withdrawal Request at
   nonce ≥ N, not just the one that triggered detection.** Reflects the
   EVM invariant that nonce N+1 can never be canonically confirmed while
   nonce N isn't — treating them independently would be wrong, not just
   incomplete.
9. **Resolving a `disputed` Withdrawal Request is always an explicit
   operator action, never automatic.** Either rebroadcast the original
   signed raw tx (safe — identical nonce and payload, no new signature,
   no double-spend risk), or submit a nonce-filler transaction to
   abandon it and trigger a Compensating Entry. This is the one case
   kept out of "fully automatic" — see Open Questions #4's resolution.
10. **A Withdrawal Request below the asset's Minimum Withdrawal Amount
    is rejected at validation, before any ledger Reservation is made.**
    Same off-ramp as any other validation failure — `rejected`, no
    Entry ever posted — so a dust request never even touches the
    Ledger or claims a Hot Wallet nonce.
11. **A Withdrawal Request's own row is created idempotently, before
    anything else runs.** `INSERT INTO withdrawal_requests
    (idempotency_key, ...) ON CONFLICT (idempotency_key) DO NOTHING`,
    checked first — a concurrent duplicate `POST /withdrawals` (client
    retry, double-click, network retry) can race to create the row, but
    only one insert wins. The loser doesn't re-validate, re-reserve, or
    call `wallet-service` again — it reads back the existing row and
    returns *its* current status. This closes the gap invariants #1–2
    already assumed: they guard against re-signing or re-reserving an
    *existing* request, but not against two requests existing in the
    first place for the same `idempotency_key`. **Decided (Tech Lead,
    Open Questions #9b, option B): a duplicate `POST` never re-validates,
    whatever the row's status — including `requested`.** Only the
    original `POST` handler, in-line, ever advances a row out of
    `requested`; no background worker re-drives it. A row left in
    `requested` (a fail-closed `503` during validation, or a crash
    mid-validation) is therefore inert, and the Reaper later marks it
    `failed` (invariant #17); the client retries with a **new**
    `idempotency_key`, which cannot double-spend because the old row can
    never move again. **Same user, same `idempotency_key`, different
    request** (`asset_id`, `amount` or `to_address` differ from the stored
    row) **returns `409`** and changes nothing — **Decided (Tech Lead),
    implemented in task 3.1**; an identical replay gets the existing
    row's status as before. Every state transition is a guarded `UPDATE ...
    WHERE status = <expected>`, so a Reaper write and a late in-line
    transition can never both succeed. (Uniqueness scope: see invariant
    #20, PROPOSED.)
12. **A Withdrawal Request's `to_address` must be an `active` Whitelist
    Entry for that user, checked at validation** — same step as
    Minimum Withdrawal Amount (invariant #10), before any Reservation.
    An address that's `pending` (still cooling down) or was never
    whitelisted at all is rejected the same way as any other validation
    failure — `rejected`, no Entry ever posted. (See also invariant
    #18: the entry's address must additionally not be one of our own
    deposit addresses.)
13. **Adding a Whitelist Entry never takes effect immediately.** Created
    `pending`; only usable once Whitelist Cooldown has elapsed. Removing
    a Whitelist Entry takes effect immediately, no cooldown — a cooldown
    protects against *gaining* a new withdrawal destination, not against
    losing one, so there's no symmetric risk to guard on the remove
    path.
14. **A Withdrawal Request's `to_address` is also checked against the
    Screening List** (`compliance-screening.md`) at the same validation
    step, alongside the whitelist and minimum-amount checks — a match
    rejects the request before any Reservation.
15. **A Whitelist Entry's `(user_id, address)` pair is unique.**
    **Decided** (`docs/design/integration-contracts.md` Open Questions
    #10). `UNIQUE(user_id, address)` on `whitelist_entries` —
    `INSERT ... ON CONFLICT (user_id, address) DO NOTHING`. Re-adding an
    address that already has a `pending` or `active` entry for that
    user is a no-op: it neither errors nor restarts that entry's
    Whitelist Cooldown clock.
16. **Hot Wallet signing is idempotent per `idempotency_key` —
    `wallet-service` never signs twice for the same key.** Strengthens
    invariant #6: atomic nonce allocation alone prevents two *different*
    calls from ever colliding on the same nonce, but it does not prevent
    one caller's *retried* call — e.g. after the HTTP response to a
    successful sign was lost in transit (timeout, connection reset) —
    from being treated as a brand-new request and allocated a
    *different* nonce for what should have been the same transaction.
    That strands the first nonce forever, because EVM's strict
    per-account nonce ordering means a later nonce can never confirm
    while an earlier one from the same account is missing — the exact
    same failure shape as a Nonce Gap (invariants #7–9), just caused by
    a lost response instead of a reorg, and happening at the point of
    allocation rather than being detected later. `wallet-service`
    persists `(idempotency_key -> nonce, signed_raw_tx)` the first time
    it signs for a given key; every subsequent `POST /v1/sign/hot-wallet`
    call carrying that same key returns the identical stored
    `{nonce, signed_raw_tx}` instead of allocating anything new or
    signing again. The `idempotency_key` used here is the *same* key the
    Withdrawal Request already carries throughout its lifecycle
    (**Decided, invariant #20: the Withdrawal Key
    `withdrawal:{withdrawal_id}`, not the client's key**)
    (Ubiquitous language > *Withdrawal Request*) — reused as-is on this
    call, never minted fresh per signing attempt. The identical
    requirement applies to this endpoint's other two callers — the
    Treasury Sweep caller (`wallet-tiering.md`, `withdrawal-service`'s
    sweep-monitor loop) and the Gas Top-Up caller (`sweeper`,
    `deposit-flow.md`) — which have no pre-existing per-request key the
    way a Withdrawal Request does; see those docs for their **PROPOSED**
    key-minting schemes.
17. **A Lock does not stay `active` forever.** Without this, a
    Withdrawal Request that crashes mid-flight (before broadcast) or
    whose broadcast tx never lands leaves its Lock's reserved capacity
    permanently unavailable to the user — the Available Balance
    invariant (`ledger.md` #1) would technically hold, but the user's
    funds would be frozen with no path back. A periodic **Reaper** loop
    in `withdrawal-service` reclaims stuck requests, but only ones it can
    *prove* are safe to reclaim — resolves Open Questions #3:
    - `requested` for longer than the **Stuck Request Timeout**
      (**Decided (Tech Lead, Open Questions #9b): 5 minutes**, the same
      value as the Stuck Reservation Timeout): no
      Lock was ever created, so the Reaper only marks the row `failed`
      (terminal, guarded `WHERE status='requested'`), no ledger call. The
      client must use a new `idempotency_key` (invariant #11).
    - `validated` for longer than the Stuck Reservation Timeout
      (**Decided (Tech Lead), Open Questions #14**): a crash between
      `lock()` succeeding and the `validated -> reserved` transition
      leaves an active Lock behind a `validated` row that no rule above
      covered. Treated like `reserved`: Release (idempotent; a `404`
      "never locked" is fine) then `failed`.
    - `reserved` or `signed` for longer than the **Stuck Reservation
      Timeout** (**Decided (Tech Lead): 5 minutes** — Open
      Questions #3): no `tx_hash` exists yet in either state, so nothing
      was ever broadcast — always safe to Release. The Reaper Releases
      the Lock (`ledger.md`) and marks the request `failed` terminally,
      so a `signed`-but-never-broadcast raw tx can never be broadcast by
      any later code path after its Lock is gone.
    - `broadcast` for longer than the **Stuck Broadcast Timeout**
      (**Decided (Tech Lead): 128 blocks**, ~25.6 min on Sepolia —
      Open Questions #3) **and** a live
      `eth_getTransactionCount(hot_wallet, "pending")` read shows the Hot
      Wallet's on-chain nonce has already advanced past this tx's nonce
      — positive proof some *other* transaction consumed that nonce, so
      this exact tx was replaced or dropped and can never land. Safe to
      Release, same as above. This is the same proof technique the Nonce
      Gap design already relies on (nonce comparison against the live
      chain), applied to "never confirmed" instead of "confirmed then
      reorged out."
    - `broadcast` past the Stuck Broadcast Timeout but the on-chain nonce
      has **not** advanced past it: the tx could still be sitting in
      mempool, or the network could be stuck — the Reaper has no proof
      it's dead, so it only raises an operator alert (identical posture
      to invariant #9's Nonce Gap resolution: never auto-resolve what
      can't be proven). An operator can then submit a nonce-filler (same
      mechanism as Nonce Gap recovery) to force it dead, after which the
      Reaper's second case above fires and Releases it.
18. **A Withdrawal Request's `to_address` must not be one of our own
    addresses: a user deposit address, the Hot Wallet, or the Warm
    Wallet.** **Decided (Tech Lead):** the deposit-address rule (option
    (b), `deposit-flow.md` Open Questions #13), the `CN=withdrawal-service`
    grant on the existing `GET /v1/addresses/{address}` (Open Questions
    #9a), the `422 destination_is_deposit_address` code (#9c), and the
    Hot/Warm rule (#9d) with its `422 destination_is_system_wallet` code.
    **Cold Wallet addresses are deliberately not rejected** (Decided,
    Tech Lead). Warm address source: **Decided** (Open Questions #10) —
    `GET /v1/wallet/system-addresses`. Why: a
    Hot Wallet Outflow is never a deposit (`deposit-flow.md` invariant
    #17), so a withdrawal into a deposit address would debit the
    requester, credit nobody and strand the funds; a withdrawal to the
    Hot Wallet is a self-send that debits the user for nothing; one to
    the Warm Wallet silently moves the user's money into treasury.
    Rules:
    - **Deposit addresses:** `withdrawal-service` calls `wallet-service`'s
      `GET /v1/addresses/{address}` (mTLS, `CN=withdrawal-service`).
      `200` = one of ours -> reject. `404` = not ours -> continue. **Any
      other outcome (timeout, connection error, 5xx, 403) fails closed:**
      never treated as "not ours" — the same contract as `chain-watcher`'s
      `deposit-flow.md` invariant #1 (a wrong "no" loses money silently;
      a wrong "retry" only costs time). Live call on every check, **no
      negative cache** (unlike `chain-watcher`'s, which only delays a
      deposit). TOCTOU: a deposit address is issued before anyone can
      send to it and is not guessable, so an address that is `404` now
      cannot realistically become `200` before broadcast, and a `200`
      never stops being ours; the only real hazard is a cached "no",
      which this forbids.
    - **Hot and Warm Wallet addresses (Decided, Open Questions #10):**
      both from one call, `GET /v1/wallet/system-addresses` → `{chain_id,
      hot, warm}` (mTLS, `CN=withdrawal-service`), fetched once at
      startup and cached — same rule as `wallet-tiering.md` invariant #8
      and `deposit-flow.md` invariant #15, never an env var.
      `withdrawal-service` does not accept `POST /v1/withdrawals` or
      `POST /v1/whitelist` until it has both (fail closed, like
      `chain-watcher`'s invariant #17 startup rule). `hot` is derived by
      `wallet-service` from the `hot_wallet` seed; `warm` comes from the
      operator file `infra/treasury.yaml`, read by `wallet-service` only
      (`wallet-tiering.md` invariant #9). The Warm seed is still never in
      any always-on service.
    - **Comparison:** byte-wise address equality (never string/checksum
      casing). Hot and Warm are local, in-memory comparisons.
    - **Where it runs:** after well-formed address, asset registered,
      Minimum Withdrawal Amount (#10), active Whitelist Entry (#12) and
      Screening List (#14); then the Hot/Warm comparison (local, cheap);
      then the deposit-address lookup last (the only network call);
      then the ledger Reservation. Every check precedes any Lock or Hot
      Wallet nonce claim (same off-ramp guarantee as #10).
    - **What the client sees:** the request goes to `rejected` (terminal,
      no Lock) and `POST /v1/withdrawals` returns `422` with reason
      `destination_is_deposit_address` (Decided) or, for Hot/Warm,
      **`destination_is_system_wallet`** (**Decided** — one code for both
      tiers, so it does not reveal which treasury address was hit). A
      Cold Wallet address is not rejected (Decided).
      While the lookup dependency is unavailable the response is `503`
      and the row stays `requested`; per invariant #11 (Decided, option B)
      that `idempotency_key` is spent — a retry with it only reads back
      `requested` until the Reaper marks it `failed` (invariant #17) — so
      the `503` body tells the client to retry with a **new**
      `idempotency_key`.
    - **Whitelist interaction — enforced at both points, request time
      authoritative.** `POST /v1/whitelist` applies the same three
      checks and refuses such an address up front with the same `422`
      codes (fail closed: `503`, never "create it anyway"), so a user
      does not wait out the 24h Whitelist Cooldown for an unusable entry.
      The request-time check is the one that counts, because entries
      that predate this rule must still be caught.
19. **`withdrawal-service` tells `wallet-service` about a Nonce Gap
    through one endpoint, and only an operator can lift the freeze.**
    **Decided (Tech Lead), Open Questions #12 (main question resolved;
    two operator-procedure sub-questions still open).** Closes a real gap: invariants #7-#8
    require the freeze to be central in `wallet-service`, and
    `wallet-service` already stores it (`hot_wallet_nonces.frozen_from_nonce`,
    `LEAST` semantics, `storage.FreezeNonceGap`; `POST
    /v1/sign/hot-wallet` returns `503` while it is set), but no
    interface existed for the detector to set it.
    - **Endpoint:** `POST /v1/wallet/nonce-gap` on `wallet-service`'s mTLS
      listener, body `{"chain_id", "from_nonce"}` → `200 {"chain_id",
      "frozen_from_nonce", "frozen_at"}` returning the **effective**
      freeze (the lower of any existing value and `from_nonce`). `400`
      malformed; `404` no Hot Wallet nonce row for that chain (nothing
      was ever signed, so no gap can exist); `422` if `from_nonce >=
      next_nonce` (a nonce never allocated cannot be a gap — rejects a
      buggy or hostile caller instead of freezing on nonsense).
    - **Allow-list:** `CN=withdrawal-service` only — it is the only
      component that tracks Hot Wallet transactions to Withdrawal
      Confirmation Depth and so the only Nonce Gap detector. `sweeper`
      does not detect gaps and is not on the list.
    - **Idempotency:** naturally idempotent, no key — `LEAST` and
      `COALESCE(frozen_at, now())` make a repeat with the same
      `from_nonce` a no-op, and a lower one only ever widens the freeze
      (invariant #8). Safe to retry until `200`; `withdrawal-service`
      marks the affected requests `disputed` first (its own DB), then
      calls this, retrying on failure.
    - **While frozen:** every *new* Hot Wallet signature is refused
      (`503`) for every caller; an idempotent replay of an
      already-signed key still returns its stored result (the stored
      signature is looked up before the freeze check). `sweeper`: Gas
      Top-Ups fail with `503` (transient), so after `MAX_ATTEMPTS` the
      deposit is `ALERT`-logged and skipped (`deposit-flow.md` invariant
      #16) — safe, the credit is already in the ledger; native Sweeps are
      unaffected (they sign with the user's key and nonce, not the Hot
      Wallet's). Treasury Sweeps stall the same way. New withdrawals
      reach `reserved`, cannot be signed, and the Reaper Releases them
      after the Stuck Reservation Timeout — safe, but see Open
      Questions #13.
    - **Unfreeze:** **no endpoint.** An operator clears it by a manual DB
      action on `wallet-service`'s Postgres (`UPDATE hot_wallet_nonces
      SET frozen_from_nonce = NULL, frozen_at = NULL WHERE chain_id = ?`,
      the same statement `storage.ResolveNonceGap` runs), after resolving
      each `disputed` request per invariant #9 — matching the plan's
      "operator resolution is a manual DB action, no UI yet". Keeping it
      off the mTLS listener means no service certificate, including a
      compromised `withdrawal-service`, can ever lift a freeze.
20. **A client's `idempotency_key` is scoped to that user and never
    leaves `withdrawal-service`; every downstream key is the
    server-minted Withdrawal Key.** **Decided (Tech Lead), Open
    Questions #11.** Fixes a design bug:
    `UNIQUE(idempotency_key)` was global over a client-supplied value, so
    (a) user B posting a key user A already used read back A's row,
    status and `tx_hash`; (b) `GET /v1/withdrawals/{idempotency_key}`
    let any authenticated user read any user's withdrawal by key; (c)
    the same client string was reused verbatim as the ledger Lock key and
    the `wallet-service` signing key, a namespace shared with `sweeper`'s
    `gas-topup:...` keys — a user choosing a key equal to a future Gas
    Top-Up key would make that top-up `409` forever. Rules:
    - `withdrawal_requests` gets `withdrawal_id UUID PRIMARY KEY`
      (server-minted) and `UNIQUE(user_id, idempotency_key)`; the
      invariant #11 insert becomes `ON CONFLICT (user_id,
      idempotency_key) DO NOTHING`. The same key from two users is two
      requests.
    - `GET /v1/withdrawals/{idempotency_key}` looks up `(authenticated
      user_id, idempotency_key)`; another user's key is `404`,
      indistinguishable from a key that does not exist.
    - Withdrawal Key `withdrawal:{withdrawal_id}` is used for `lock` /
      `settle` / `release`, for `POST /v1/sign/hot-wallet`, and as
      `withdrawal:{withdrawal_id}:refund` for a Compensating Entry. It is
      recomputable from the row, so retries after a crash use the same
      key, exactly as before.
    - `withdrawals.broadcast`/`withdrawals.confirmed` add `withdrawal_id`
      and use it as the Kafka message key and the consumer dedup key
      (`history-service` currently keys its document on
      `withdrawal:<idempotency_key>` — a code change, plan task 3.11).

## Diagram

```mermaid
sequenceDiagram
    participant Gateway as api-gateway
    participant WD as withdrawal-service
    participant Ledger as ledger-service
    participant Wallet as wallet-service
    participant RPC as EVM testnet RPC
    participant Kafka

    Note over Gateway: Authorization: Bearer <api_key> already resolved<br/>to an authenticated user_id — identity-auth.md
    Gateway->>WD: POST /withdrawals<br/>{idempotency_key, user_id, asset, amount, to_address}<br/>user_id is server-injected, never client-supplied
    WD->>WD: INSERT withdrawal_requests (idempotency_key, ...)<br/>ON CONFLICT (idempotency_key) DO NOTHING
    alt row already existed (duplicate request)
        WD->>WD: read back existing row
        WD-->>Gateway: existing request's current status
    else new row created
        WD->>WD: validate (well-formed address, asset registered,<br/>amount >= asset.min_withdrawal_amount,<br/>to_address is an active Whitelist Entry,<br/>to_address not on Screening List,<br/>to_address not a deposit address / Hot / Warm — invariant #18)
        WD->>Ledger: lock(idempotency_key, user_id, asset_id, amount)<br/>Reservation — reserves capacity only, no Entry posted (ledger.md)
        alt insufficient available funds
            Ledger-->>WD: error
            WD->>WD: status = rejected
            WD-->>Gateway: 422 insufficient funds
        else locked
            Ledger-->>WD: ok
            WD->>WD: status = reserved
            WD->>Wallet: sign({to_address, asset_id, amount, chain_id, idempotency_key})<br/>Hot Wallet only — no caller-chosen key
            alt idempotency_key already seen (retry after a lost response)
                Wallet-->>WD: same stored {signed_raw_tx, nonce} (invariant #16) — no new nonce allocated
            else first call for this idempotency_key
                Wallet->>RPC: fetch gas price / estimate gas limit (PROPOSED — wallet-service-owned tx construction, see integration-contracts.md §2)
                Wallet->>Wallet: allocate next Hot Wallet nonce atomically (invariant #6)<br/>persist (idempotency_key -> nonce, signed_raw_tx) (invariant #16)
                Wallet-->>WD: {signed_raw_tx, nonce}
            end
            WD->>WD: status = signed
            WD->>RPC: broadcast(raw tx)
            RPC-->>WD: tx_hash
            WD->>WD: status = broadcast
            WD->>Kafka: publish withdrawals.broadcast
            loop poll for confirmations
                WD->>RPC: get tx receipt / confirmation count
            end
            RPC-->>WD: confirmed
            WD->>Ledger: settle(idempotency_key)<br/>Settle — Lock resolves, real debit Entry posted for the first time (ledger.md)
            Ledger-->>WD: ok (settled)
            WD->>WD: status = confirmed
            WD->>Kafka: publish withdrawals.confirmed
        end
    end
```

Nonce Gap detection and recovery (invariants #7–9):

```mermaid
sequenceDiagram
    participant WD as withdrawal-service / sweeper
    participant RPC as EVM testnet RPC
    participant Wallet as wallet-service
    participant Op as Operator

    loop periodic re-check of confirmed Hot Wallet txs
        WD->>RPC: is the tx at nonce N still in the canonical chain?
        alt still included
            Note over WD: no action
        else missing — Nonce Gap at N
            WD->>WD: mark every Withdrawal Request at nonce >= N as disputed
            WD->>Wallet: report Nonce Gap at N
            Wallet->>Wallet: refuse new Hot Wallet nonce allocations
            WD->>Op: alert
            Op->>RPC: rebroadcast original raw tx at nonce N<br/>OR submit a nonce-filler tx + trigger Compensating Entry
            Op->>Wallet: confirm gap cleared
            Wallet->>Wallet: resume nonce allocation
        end
    end
```

Nonce Gap freeze and operator unfreeze (invariant #19, **Decided**):

```mermaid
sequenceDiagram
    participant WD as withdrawal-service
    participant WPG as withdrawal-service Postgres
    participant Wallet as wallet-service
    participant Op as Operator
    participant Sweeper as sweeper

    WD->>WD: detect Nonce Gap at N (tx at nonce N no longer canonical)
    WD->>WPG: mark requests with nonce >= N disputed (invariant #8)
    WD->>Wallet: POST /v1/wallet/nonce-gap {chain_id, from_nonce: N} — mTLS, CN=withdrawal-service
    Wallet->>Wallet: frozen_from_nonce = LEAST(existing, N)
    Wallet-->>WD: 200 {frozen_from_nonce, frozen_at} (retried until 200; idempotent)
    Sweeper->>Wallet: POST /v1/sign/hot-wallet (Gas Top-Up, new key)
    Wallet-->>Sweeper: 503 frozen — transient; ALERT + skip after MAX_ATTEMPTS
    Op->>Op: resolve each disputed request (rebroadcast or nonce-filler, invariant #9)
    Op->>Wallet: manual SQL on wallet-service Postgres:<br/>SET frozen_from_nonce = NULL (no endpoint)
```

Whitelist Entry lifecycle (invariants #12–13, #15; creation also refuses a deposit / Hot / Warm address, #18):

```mermaid
sequenceDiagram
    participant Gateway as api-gateway
    participant WD as withdrawal-service

    Note over Gateway: user_id resolved from Authorization header — identity-auth.md
    Gateway->>WD: add whitelist entry (user_id, address)
    WD->>WD: INSERT whitelist_entries (status=pending, added_at=now())<br/>ON CONFLICT (user_id, address) DO NOTHING
    Note over WD: not usable for withdrawal yet<br/>(re-adding an existing pending/active entry no-ops, invariant #15)

    Note over WD: 24h Whitelist Cooldown elapses

    Note over WD: on next validate() call for this user,<br/>now() >= added_at + 24h → entry counts as active<br/>(computed on read, no background job)
```

## Alternatives considered

- **Pay out from each user's own derived deposit address (no Hot
  Wallet / no sweep).** Rejected: every user address would need its own
  maintained native-coin gas balance before it could pay out anything —
  true even for ERC-20 withdrawals, since gas is always paid in native
  coin by the sending address. Not how custody systems are actually
  run at any real scale. Recommending the Hot Wallet / sweep model
  instead — see Open Questions #1, since it also means a small addition
  to `deposit-flow.md`.
- **Manual/threshold review queue.** This is the alternative to "fully
  automatic" you already decided against; not re-litigating it. I am
  flagging one narrow exception in Open Questions #4 — confirmed-then-
  reorged withdrawals — as a case I'd keep out of full automation
  regardless.
- **`withdrawal-service` holds its own key material instead of calling
  `wallet-service`.** Rejected outright, not seriously considered — it
  directly violates this project's stated security model (one service
  holds key material, never exposes private keys).
- **Block the HTTP response until the withdrawal confirms.** Rejected —
  testnet confirmation takes real wall-clock time. The request returns
  quickly with a request ID; confirmation is tracked asynchronously,
  consistent with how the rest of this system is event-driven.
- **Auto-rebroadcast on Nonce Gap detection, no operator involved.**
  Rebroadcasting the *same* signed tx is provably safe, so this was
  tempting — but a Nonce Gap can also mean the operator (or a future
  policy) wants to change what happens at that nonce (different gas
  price, abandon it entirely), and auto-resolving removes that choice
  right when something already went wrong in an unusual way. Rejected —
  matches your standing decision to keep this one case out of full
  automation.
- **Treat each disputed Withdrawal Request independently instead of
  freezing the whole nonce ≥ N range.** Rejected — it's not just simpler,
  it's wrong: EVM won't canonically confirm nonce N+1 while nonce N
  isn't confirmed, so treating them independently would let
  `withdrawal-service`/`sweeper` keep issuing transactions that can
  never actually land.
- **No cooldown, whitelist entries active immediately.** Rejected —
  this is the whole point of a whitelist. Without a delay, an attacker
  who compromises a user's session can add their own address and
  withdraw to it in the same request; the whitelist would add an extra
  API call for a real user's convenience while doing nothing for
  security.
- **Let users disable the whitelist requirement entirely** (some real
  exchanges offer this as an opt-out for convenience, at higher risk).
  Not offering this for v1 — matches the project's existing conservative
  stance (fully automatic withdrawals, mandatory Minimum Withdrawal
  Amount) of not giving users a lever to weaken custody controls.
- **Leave `POST /v1/sign/hot-wallet` without an idempotency key and rely
  solely on invariant #2's `status='signing' WHERE status='reserved'`
  guard, as originally designed.** Rejected on Tech Lead review: that
  guard only prevents `withdrawal-service` from *asking* twice in the
  normal case; it does nothing if the first call's response is simply
  lost (timeout/connection reset) after `wallet-service` already signed
  server-side — a retry (by `withdrawal-service` itself, an operator, or
  a retry mechanism) would then be indistinguishable from a genuinely
  new request and would allocate a second, stranding nonce. Invariant
  #16 (idempotency key persisted alongside the signed result) closes
  this without weakening invariant #2, which is still needed for the
  ordinary "don't sign a request that's still `reserved`" case.
- **Debit the user's Account at Reservation time instead of
  Lock-then-Settle** (this doc's original design). Rejected on review:
  it meant every pre-broadcast failure — by far the common case — needed
  a Compensating Entry just to undo something that never actually left
  custody, touching `ledger.md`'s immutable Entries table for money that
  never moved. The Lock/Settle/Release split (`ledger.md`) reserves
  capacity without pretending funds moved until a Withdrawal Request
  actually reaches `confirmed`, and confines Compensating Entries to the
  one case that genuinely needs one: a `disputed` withdrawal abandoned
  after it had already Settled (invariant #9). See the revised
  invariant #3 and Compensating Entry definition above.

## Open questions

1. **Sweep vs. direct model — the biggest open item.** I'm recommending
   user deposit addresses stay attribution-only, and a Sweep step
   (small addition to `deposit-flow.md`, not written yet) consolidates
   confirmed deposits into the Hot Wallet per asset. All withdrawals pay
   out from there. This is standard custodial-exchange practice and
   sidesteps per-address gas funding. I didn't fold this into
   `deposit-flow.md` silently since it changes that doc's shape — want
   your sign-off first, then I'll add that section.
   **Decided (PR #1): approved.** The Sweep section is now written up
   in `deposit-flow.md`, including the gas-funding mechanics.
2. **`withdrawal-service` ↔ `wallet-service` auth.** This is new surface
   that can move funds. Options range from "same Docker network, no
   auth, testnet-only" (matches the project's current low-ceremony local
   setup) to a shared bearer secret or mTLS. This is a security-posture
   call, not something I'll default silently.
   **Decided (PR #1): mTLS.** Full CA/certificate-issuance and
   CN-based authorization design (the "which caller may call which
   endpoint" piece this decision didn't originally cover):
   `docs/design/service-auth-mtls.md`.
3. ~~**Stuck/dropped broadcast handling / stuck Locks.** A withdrawal
   that's broadcast but never confirms (underpriced, replaced, dropped
   from mempool) is the trickiest failure mode here — Releasing its Lock
   safely (per the revised invariant #3/#4) requires certainty it's
   truly dead, not just slow. Also raised independently on review: a
   Lock that never resolves at all (crash before broadcast, or a
   genuinely stuck tx) leaves the user's funds frozen forever with no
   mechanism to reclaim them — someone has to release it eventually.~~
   **Decided: mechanism resolved, exact numbers still Tech Lead's call.**
   A periodic Reaper (new invariant #17) reclaims stuck requests using
   the same proof technique the Nonce Gap design already established —
   compare the Hot Wallet's live on-chain nonce against the stuck tx's
   nonce, never a bare timeout alone: `reserved`/`signed` (no `tx_hash`
   yet) is always safe to auto-Release once past a **Stuck Reservation
   Timeout**; `broadcast` is safe to auto-Release once past a **Stuck
   Broadcast Timeout** *and* the on-chain nonce has moved past it
   (proof this exact tx was replaced/dropped, not just slow); otherwise
   the Reaper alerts an operator rather than guessing. Two numbers are
   still open, same shape as `wallet-tiering.md`'s Hot Floor/Ceiling
   split (mechanism decided here, absolute values are a risk-tolerance
   call): **Stuck Reservation Timeout** (recommend something short and
   generous, e.g. a few minutes — nothing was ever broadcast in this
   state, so there's no cost to reclaiming quickly) and **Stuck
   Broadcast Timeout** (recommend at least Withdrawal Confirmation
   Depth's 64-block window plus margin, e.g. 128 blocks (~25.6 min), so
   the Reaper never races a tx that's simply slow to confirm). Confirm
   these two values, or set your own — the mechanism doesn't change
   either way. This is a separate question from Withdrawal Confirmation
   Depth (resolved, see below) — Confirmation Depth is about how long to
   wait once a tx *is* included before calling it final; this was about
   what to do when a broadcast tx never gets included at all, or a
   request never gets that far.
   **Decided (Tech Lead): Stuck Reservation Timeout = 5 minutes, Stuck
   Broadcast Timeout = 128 blocks.** Invariant #17 updated.
4. ~~Confirmed-then-reorged withdrawal.~~ **Decided (recommendation
   accepted):** not automated. Full design: a **Nonce Gap** (a Hot
   Wallet tx at nonce N reorged out) invalidates every Hot Wallet tx at
   nonce ≥ N too, by EVM's own nonce-ordering rule — so this isn't just
   about one withdrawal, it can stall `withdrawal-service` and `sweeper`
   both, since they share one Hot Wallet nonce sequence. On detection:
   `wallet-service` stops allocating new nonces (invariant #7), every
   affected Withdrawal Request moves to `disputed` (invariant #8), and
   an operator resolves it explicitly (invariant #9) — either
   rebroadcasting the original signed tx (safe: identical nonce and
   payload, no double-spend risk) or abandoning it with a nonce-filler
   tx plus a Compensating Entry. See the new invariants #7–9, the
   Nonce Gap / Disputed terms above, and the recovery diagram. Also
   recommending **Withdrawal Confirmation Depth be deeper than
   deposit-flow.md's Confirmation Depth**, since funds leaving custody
   are more sensitive than funds arriving. **Depth values now resolved:**
   Sepolia is the chosen testnet — Withdrawal Confirmation Depth = 64
   blocks (~12.8 minutes), double deposit-flow.md's 32-block
   Confirmation Depth. See the Withdrawal Confirmation Depth entry in
   Ubiquitous language above for the full rationale.
5. ~~Dust/minimum withdrawal amount~~ **Decided:** see Minimum
   Withdrawal Amount above and invariant #10 — 15× gas cost for native
   coin, operator-set for ERC-20, enforced at validation. **Still
   open:** whether the Hot Wallet's own gas balance needs low-balance
   alerting — not designing now, flagging for the observability phase
   (roadmap step 3).
6. **Withdrawal Whitelist Cooldown: Decided (24h).** One honest gap
   worth flagging: the cooldown's security value depends on the user
   actually noticing a new `pending` entry before it activates, but
   this project has no notification/email service (not on the roadmap
   yet). Without one, the 24h window is real but silent — a user only
   discovers an unauthorized addition by manually checking their
   whitelist page. Do you want a minimal notification mechanism in
   scope now, or accept this as a known gap until `history-service`/
   notifications exist?
7. ~~How is "add a Whitelist Entry" itself authenticated?~~ **Decided,
   resolved:** see `identity-auth.md` — every `api-gateway` request
   (whitelist management included) now requires a valid API Key, and
   `user_id` is server-injected from it, never client-supplied. This
   was flagged as inherited from a project-wide gap; that gap is what
   `identity-auth.md` closes.
8. **Decided (Tech Lead, 2026-09-29): yes, and yes to `max_gas_price`.**
   Original proposal: should `wallet-service` own full EVM
   transaction construction (gas price/limit, via its own EVM RPC
   client), rather than the caller supplying gas parameters?** This is
   a real, previously-unresolved gap — the signing request shape never
   said how gas gets set. Recommending `wallet-service` own it (invariant
   #5 above), since it keeps "caller can't influence how the tx is built
   beyond what it's for" true for gas the same way it's already true for
   the destination key. This is a genuine new dependency, though: it
   means `wallet-service` needs its own EVM RPC client, which
   `docs/ARCHITECTURE.md`'s `wallet-service` row doesn't list today
   (proposed addition there too, marked pending). Full design and the
   request/response shape this implies: `docs/design/integration-contracts.md`
   §2. Please confirm this is the right call before Milestone 0 work on
   the signing endpoint starts — the alternative (caller estimates gas,
   passes it in) is architecturally simpler but weakens the
   invariant-#5 security property and was not silently chosen instead.
   Also open within the same proposal: whether to accept an optional
   caller-supplied `max_gas_price` ceiling (a cost-exposure cap, not a
   dictated price) — recommending yes, but flagging it as a separate
   yes/no since it could reasonably be deferred to the observability
   phase instead.
9. **Resolved (Tech Lead) — all four sub-questions decided; invariant
   #18 updated.** (a) reuse `GET /v1/addresses/{address}` and grant
   `CN=withdrawal-service`; (b) option B: invariant #11 unchanged, the
   Reaper fails stale `requested` rows (timeout 5 min, Decided),
   client uses a new key; (c) keep `422 destination_is_deposit_address`;
   (d) also reject the Hot and Warm Wallet addresses with `422
   destination_is_system_wallet` (Decided); Cold Wallet addresses are
   not rejected (Decided); Warm source Decided, #10. Original text:
   ~~**New (Tech Lead resolved `deposit-flow.md` Open Questions #13 as
   option (b); invariant #18 is the design).** The rule is decided; these
   details are **PROPOSED** for confirmation:
   (a) **New authz grant:** `CN=withdrawal-service` on `wallet-service`'s
   `GET /v1/addresses/{address}` (today `chain-watcher` only). Note the
   endpoint returns `user_id`, which `withdrawal-service` does not need;
   a compromised `withdrawal-service` could use it to map addresses to
   users. Alternative for least privilege: a separate existence-only
   endpoint (e.g. `HEAD /v1/addresses/{address}`, `204`/`404`, own
   allow-list). I kept the existing endpoint to avoid a second one for
   a testnet system — your call.
   (b) **Where a transient failure leaves the request:** invariant #18
   returns `503` and does not mark the request `rejected`, leaving the
   row in `requested`. But invariant #11 says a duplicate `POST` reads
   back the existing row without re-validating, so as written a request
   stuck in `requested` would never be retried and the Reaper (#17)
   covers only `reserved`/`signed`/`broadcast`. Proposal: a duplicate
   `POST` for a row still in `requested` re-runs validation (side-effect
   free, so safe), refining #11. Needs a decision.
   (c) **Error disclosure:** the distinct `destination_is_deposit_address`
   reason tells a user that an address they typed belongs to this
   system. Low sensitivity (validation only reaches it for addresses
   the user already whitelisted, and creation-time refusal lets them
   probe the same way); flagging in case you prefer a generic
   "destination not allowed".
   (d) **Not covered:** the Hot Wallet's own address and the Warm Wallet
   address as withdrawal destinations are not addressed here.~~
10. ~~**Open: where does the Warm Wallet address come from?** Needed by
    invariant #18 (reject withdrawals/whitelist entries to it) and —
    an existing, previously unnoticed gap — by the Treasury Sweep itself
    (`wallet-tiering.md`'s diagram signs `to=Warm address` with no
    designed source). `wallet-service` cannot derive it (Warm's seed is
    never in an always-on service). It is public, not secret, but it is
    operator-chosen, so it must come from config somewhere. My
    recommendation: one canonical operator config file (same pattern as
    `infra/assets.yaml`, e.g. `infra/treasury.yaml` with the Warm (and
    Cold) address per chain), loaded by `wallet-service` only and served
    by extending `GET /v1/wallet/hot-address` into e.g. `GET
    /v1/wallet/system-addresses` → `{hot, warm}`; `wallet-service` could
    then also enforce server-side that a Treasury Sweep's destination is
    Warm. Alternatives: the same file mounted into `withdrawal-service`
    directly, or an env var (rejected by default — drift, per
    `deposit-flow.md` invariant #15). Cold: **Decided (Tech Lead) — Cold
    Wallet addresses are not rejected as withdrawal destinations**, so
    only the Warm address is needed from this source.~~
    **Decided (Tech Lead): approved as recommended.** One operator file,
    `infra/treasury.yaml`, read by `wallet-service` only, served by a new
    `GET /v1/wallet/system-addresses` → `{chain_id, hot, warm}` (mTLS,
    `CN=withdrawal-service` only) that sits **beside** `GET
    /v1/wallet/hot-address` (unchanged, still used by `sweeper` and
    `chain-watcher`). `wallet-service` fails startup if the file or the
    Warm address for its chain is missing or malformed. Full rules:
    `wallet-tiering.md` invariant #9.
11. ~~**PROPOSED: per-user idempotency keys and a server-minted Withdrawal
    Key (invariant #20).** Changes `withdrawal-flow.md`,
    `integration-contracts.md` §1/§2/§3/§4 and `ledger.md` (key source
    only — `ledger-service`'s code is unaffected, it takes any string
    up to 256 chars). Touches built code in two places: `pkg/events`
    (add `withdrawal_id` to both withdrawal events) and
    `history-service` (key on it). Needs approval before M3 starts,
    since task 3.1's schema depends on it.~~ **Decided (Tech Lead):
    approved as proposed.**
12. ~~**PROPOSED: Nonce Gap freeze endpoint `POST /v1/wallet/nonce-gap`
    (invariant #19), `CN=withdrawal-service` only, no unfreeze
    endpoint.**~~ **Decided (Tech Lead): approved as proposed** — manual
    operator SQL to unfreeze. **Still open** underneath it: after an operator resolves a
    gap, does `wallet-service`'s `next_nonce` need resetting (e.g. to the
    chain's pending nonce)? `storage.ResolveNonceGap` deliberately leaves
    it alone because no doc says; if the operator used nonce-fillers
    signed outside `wallet-service`, `next_nonce` can be behind the
    chain. And how nonce-fillers are signed at all (the Hot key is only
    in `wallet-service`) is not designed. Both belong to invariant #9's
    operator procedure.
13. **Open, minor: should `withdrawal-service` refuse new withdrawals
    while a Nonce Gap is open,** instead of reserving funds that the
    Reaper then Releases 5 minutes later (`failed`)? Safe either way;
    refusing up front (`503`) is kinder but needs `withdrawal-service`
    to track its own "frozen" flag. Not designed.
14. ~~**PROPOSED addition: the Reaper also covers `validated`** (invariant
    #17) — a crash after `lock()` succeeded but before `reserved` left
    an active Lock no rule covered. Confirm.~~ **Decided (Tech Lead):
    yes.** Also Decided: Stuck Request Timeout = 5 minutes.
15. **Backlog, not M3 (raised by the Tech Lead): several Hot Wallets as
    backup**, so a Nonce Gap freeze on one does not stop all
    withdrawals. Out of scope here; tracked in `IMPLEMENTATION_PLAN.md`
    Backlog and `wallet-tiering.md` Open Questions #8.
