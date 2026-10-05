# withdrawal-service

Orchestrates withdrawals (`docs/design/withdrawal-flow.md`). **Tasks 3.1-3.6, 3.9, 3.10, 3.13-3.15 and the
withdrawal-service part of 3.11 so far:** a request is recorded idempotently, validated, Reserved on the
ledger, then signed, broadcast, confirmed and Settled in the background; confirmed txs are watched for
Nonce Gaps; an operator can abandon a disputed request (refund or Release, 3.5). A Reaper reclaims stuck
requests (3.9), and a stuck tx is unblocked by a Fee Bump or a Nonce Filler, never abandoned (3.13). The
e2e test (3.8) is not built yet.

## What works

- **Schema** (`migrations/`): `withdrawal_requests`, one row per request, with the full state list
  (`requested → validated → reserved → signed → broadcast → confirmed`, plus `rejected`, `failed`,
  `disputed`) enforced by a `CHECK`. Amounts are `NUMERIC(78,0)`.
- **Keys (invariant #20).** The client's `idempotency_key` is unique **per user**:
  `UNIQUE(user_id, idempotency_key)`. Each row also gets a server-minted `withdrawal_id` (UUID), and
  `Withdrawal.Key()` = `withdrawal:{withdrawal_id}` is the Withdrawal Key every downstream call will use
  (ledger Lock/Settle/Release, wallet-service signing). The client's key never leaves this service.
- **`POST /v1/withdrawals`** `{user_id, idempotency_key, asset_id, amount, to_address}` (mTLS,
  `CN=api-gateway` only; `user_id` is injected by the gateway):
  - The row is inserted **first**, `INSERT ... ON CONFLICT (user_id, idempotency_key) DO NOTHING`
    (invariant #11), then validated and Reserved in-line by the same handler:
    `202 {idempotency_key, status: "reserved", tx_hash: null}` once the ledger Lock holds the amount, or
    `422 {..., status: "rejected", reject_reason}` — a failed check (below) or `insufficient_funds`
    (the Lock was refused; nothing is held).
  - Duplicate (same user, same key, same request): `200` with the existing row's status. It never
    re-validates, whatever the status (Tech Lead, Open Questions #9b option B).
  - Same user and key but a **different** request (asset, amount or address): `409`. This is not a
    retry; answering with another request's status would mislead the client. Not spelled out in the
    design docs (which only cover the identical-retry case); it matches ledger-service's and
    wallet-service's handling of a reused key.
  - `503` when validation could not reach a verdict, or the ledger could not be reached for the Lock.
    The row stays `requested` (resp. `validated`) and that key is spent: retrying it only reads the row
    back. The Reaper (3.9) will fail it (and Release, for `validated`); the client retries with a new key
    (Tech Lead, Open Questions #9b option B).
  - `400` only for a body that cannot be stored (bad JSON, unknown field, missing field, a key that is
    not 1-128 printable ASCII characters, an amount that is not a positive integer in minor units).
    Everything else — asset registered, minimum, address format, whitelist, screening, destination —
    is task 3.2's validation and ends in `rejected`.
- **`GET /v1/withdrawals/{idempotency_key}?user_id=`** (mTLS, `CN=api-gateway`): the caller's own
  request, `{idempotency_key, status, tx_hash, reject_reason?}`. Another user's key is `404`, the same
  as a missing one.
- `withdrawal_id` and `user_id` are never returned.
- **Validation** (task 3.2; invariants #10, #12, #14, #18), in the decided order, first failure wins.
  Local checks first, the remote one last, so a request is never sent to wallet-service if a local rule
  already rejects it. Every guarded `requested → validated|rejected` transition is an
  `UPDATE ... WHERE status = 'requested'`; if the Reaper got there first, the response reports what it
  found.

  | # | Rule | `reject_reason` |
  |---|---|---|
  | 1 | asset is in the Asset Registry (`ASSETS_FILE`) | `asset_not_registered` |
  | 2 | `to_address` is a 0x 20-byte hex address, not zero | `invalid_to_address` |
  | 3 | amount fits in uint256 | `amount_too_large` |
  | 4 | amount ≥ the asset's `min_withdrawal_amount` | `below_minimum_withdrawal_amount` |
  | 5 | `to_address` is an **active** entry in the user's own whitelist | `destination_not_whitelisted` |
  | 6 | `to_address` is not on the Screening List (`SCREENING_FILE`) | `destination_on_screening_list` |
  | 7 | not the Hot or Warm Wallet (local comparison) | `destination_is_system_wallet` |
  | 8 | not one of our deposit addresses (wallet-service `GET /v1/addresses/{address}`) | `destination_is_deposit_address` |

  Codes and order are Decided by the Tech Lead (`withdrawal-flow.md` invariant #10).
- **Background pipeline** (task 3.3, `internal/pipeline`), one pass every `POLL_INTERVAL` (3s):
  1. `reserved → signed`: `POST /v1/sign/hot-wallet` with the **Withdrawal Key** `withdrawal:{withdrawal_id}`
     (never the client's key). The signed raw tx, its nonce and chain are stored. wallet-service returns
     the stored tx for a key it already signed (invariant #16), so a lost response or a crash never
     allocates a second nonce. A refusal (400/422: no nonce allocated) Releases the Lock and fails the
     request (`sign_refused`); a 409 (the key already signed for a different
     request, so a nonce exists) is an `ALERT` and never released (withdrawal-flow.md Open Questions #16);
     anything else (503 during a Nonce Gap, 5xx, timeout) is retried.
  2. `signed → broadcast`: sends exactly the stored raw tx. "Already known", or a tx that is already
     mined, counts as sent. If the node says the nonce is used and our tx is not mined, an `ALERT` is
     logged and the request is left alone (invariant #4: never auto-resolved once a tx exists).
  3. `broadcast → confirmed`: once the receipt is `WITHDRAWAL_CONFIRMATION_DEPTH` blocks deep (decided:
     64; 2 in `e2e/`), **Settle** — the only point where the debit Entry is posted (#3) — then the row
     records the block and the ledger transaction id (task 3.5 corrects it on a refund). A tx mined but
     **reverted** paid nothing: once the revert is itself that deep (a shallower receipt can still change),
     Release, `failed` / `tx_reverted`, no auto-retry (Decided). The revert reason is recovered by
     replaying the tx (`eth_call` at the block before) and logged; a reason that says the sender lacked
     funds — the Hot Wallet short of the asset, our fault — is logged as `ALERT`. Not yet reported to the
     user: the `withdrawals.failed` event is being designed. A Settle that conflicts (the Lock was
     released) is logged `ALERT` and left for an operator.
  4. Events: `withdrawals.broadcast` / `withdrawals.confirmed` are written to an **outbox** table in the
     same transaction as the state change, then published (Kafka key = `withdrawal_id`) and marked
     published only after the broker's ack. A crash in between republishes; nothing is lost.

  Every step is idempotent and every transition is a guarded `UPDATE ... WHERE status = ...`, so running
  a pass twice, or two instances at once, does no harm. A request that may already have a nonce is only
  ever moved forward: dropping a signed tx would strand that Hot Wallet nonce and block every later Hot
  Wallet tx.
- **Nonce Gap detection** (task 3.4, invariants #7-#9, #19), part of every pipeline pass:
  - every `confirmed` request mined in the last `NONCE_GAP_WINDOW` blocks (default 256) has its receipt
    re-read. A receipt missing on **two passes in a row** (one flaky RPC answer must not freeze all Hot
    Wallet signing) means a reorg deeper than the confirmation depth took the tx back: a Nonce Gap at its
    nonce N (the lowest such one);
  - in one transaction the gap is recorded (`nonce_gaps`) and every request on the chain at nonce ≥ N that
    holds a tx (`signed`, `broadcast`, `confirmed`) becomes `disputed`, remembering where it came from
    (`disputed_from`) — by EVM nonce ordering none of them can confirm while N is missing. `ALERT` logged;
  - then wallet-service is told (`POST /v1/wallet/nonce-gap`), retried every pass until it answers `200`.
    From that moment no new Hot Wallet signature is made for anyone: new withdrawals wait in `reserved`;
  - the pipeline never touches a `disputed` request again. Only an operator resolves it (#9).

  **Operator resolution (stub — manual DB actions, no UI yet).** For each disputed request, oldest nonce
  first, in withdrawal-service's database:
  1. *Rebroadcast* (the original tx can still land): send `signed_raw_tx` to the chain yourself, wait for
     the confirmation depth, then put the request back where it was:
     `UPDATE withdrawal_requests SET status = disputed_from, disputed_from = NULL, disputed_at = NULL, updated_at = now() WHERE withdrawal_id = '<id>' AND status = 'disputed';`
     (a `broadcast`/`signed` one then continues through the pipeline; a `confirmed` one had already Settled).
  2. *Abandon* (the tx can never land, task 3.5): first clear nonce N with a Nonce Filler (the CLI is task
     3.13), then run
     `docker compose exec withdrawal-service service -abandon <withdrawal_id>`.
     It refuses unless the chain proves the tx is dead — the tx is not mined **and** the Hot Wallet's mined
     (`latest`) nonce is past N. Then, if it had Settled (`disputed_from = confirmed`), it posts the
     **Compensating Entry**: a ledger correction of exactly the Settle's two entries (ledger-service returns
     them from the Settle), keyed `withdrawal:<id>:refund`, amounts derived by the ledger — the user gets
     back exactly what was debited, at most once. Otherwise it Releases the Lock. Either way the request
     becomes `failed` (`abandoned_refunded` / `abandoned`). Safe to re-run after a crash.
  3. When no request is `disputed` any more, lift the freeze in wallet-service's database (its README).
  The Nonce Filler is not built yet (task 3.13); until then a filler has to be sent by hand.
- **Fee Bump, pre-sign re-check, Nonce Filler** (task 3.13, invariants #21-#23):
  - A tx can have several versions: the original (`signed_raw_tx`) and Fee Bumps (`withdrawal_txs`, seq ≥ 1),
    all at one nonce, destination and amount. Only the latest is (re)sent; any version may be mined, never
    more than one. `confirm`, the Reaper's proof and `-abandon` look at **all** of them. The version that is
    mined becomes the row's `tx_hash` and is what `withdrawals.confirmed` carries, so it can differ from the
    `withdrawals.broadcast` one.
  - **When to bump:** the node refuses the tx as underpriced, or it is not mined `FEE_BUMP_AFTER_BLOCKS`
    (20) blocks after it was last sent **and** its max fee is below the current base fee. At most
    `MAX_FEE_BUMPS` (5), then an ALERT. wallet-service builds the replacement (`POST .../replace`, fees ×1.125,
    hard ceiling 50 gwei); a `422` (ceiling) or `409`/`404` is an ALERT, anything transient is retried with the
    same `replacement_seq`. The pipeline checks the replacement is the same tx (nonce, sender, destination,
    value, data, gas, higher fee) before storing it, and stores it (`sent_head` NULL = "not sent yet") *before*
    sending it, so a crash is repaired on the next pass.
  - **Pre-sign re-check** (`reserved → signed`): the Whitelist Entry must still be `active` and the address off
    the Screening List. On failure the request is Released and `failed` (`destination_not_whitelisted` /
    `destination_on_screening_list`) — but only after wallet-service has **voided** its key: a lost sign reply
    can mean a nonce already exists, in which case the request goes on (a signed tx is never blocked, only
    cancelled) and an ALERT says so. A request still `signed` after the Stuck Reservation Timeout that now fails
    the re-check also raises a Reaper ALERT.
  - **Nonce Filler:** the operator cancels a signed withdrawal with wallet-service's
    `service hot-wallet cancel --key withdrawal:<id>` (its README). When it is mined the Reaper's proof rule
    Releases the request (`failed` / `replaced_or_dropped`). A sign answered `410` (the key was voided) is
    left to the Reaper.
- **`withdrawals.failed` and Whitelist `blocked`** (task 3.14, invariants #24, #25):
  - Every `→ failed` — sign refused, tx reverted, Reaper (`stale_request`, `stale_reservation`,
    `replaced_or_dropped`), the pre-sign re-check, `-abandon` — goes through one function that commits the
    guarded `UPDATE` and the `withdrawals.failed` outbox row **in one transaction** (the store refuses a plain
    `Transition` to `failed`). The outbox row is unique per request, so a request is reported once, however
    often anything retries; Kafka key `withdrawal_id`. The event carries the reason, the tx hash and nonce when
    the request was ever signed (the mined tx for a revert, else the latest Fee Bump version), and for
    `tx_reverted` the revert reason. `rejected` publishes nothing: the client already got the `422`.
  - A `tx_reverted` that is **not our fault** blocks the user's Whitelist Entry for that destination, in the
    same transaction: the cause must be known and not the Hot Wallet short of the asset or of ETH (an unknown
    cause blocks nothing). `blocked` wins over `pending`/`active`, validation treats it as not active
    (`destination_not_whitelisted`), `GET /v1/whitelist` shows it, and other users' entries for the same address
    are untouched. Re-adding a blocked entry is a no-op; **delete + re-add** is a new `pending` entry with a new
    24h cooldown. An operator blocks (or unblocks) by SQL:
    `UPDATE whitelist_entries SET blocked_at = now() WHERE user_id = '<u>' AND address = '<EIP-55 address>';`
    (set `blocked_at = NULL` to lift it).
- **Gas Fees** (task 3.15, invariant #26, `ledger.md` invariant #12): every wei of gas a mined tx of ours
  spent is posted to the ledger exactly once (`POST /v1/ledger/gas-fees`, keyed on the tx hash, so the ledger
  also dedupes). The receipt's `gasUsed` and `effectiveGasPrice` are written **in the same transaction as the
  state change that makes the tx final** — `confirmed` (purpose `withdrawal`, posted after the Settle), `failed`
  / `tx_reverted` (`withdrawal_reverted`; the user is Released in full), or the Reaper's / `-abandon`'s release
  after a **mined Nonce Filler** (`nonce_filler`, the filler's hash from wallet-service's `GET
  /v1/sign/hot-wallet/{key}`) — so a crash cannot lose them. A separate pass then posts every unposted record
  until ledger-service answers `200` and stamps `gas_posted_at`. Only the version that was mined pays (a Fee
  Bump's unmined original posts nothing); a request that never mined a tx posts nothing; a permanent refusal
  (`400`/`409`) is an ALERT, not a retry loop; if wallet-service cannot say which fillers exist, the Reaper's
  Release waits for the next pass rather than lose a filler's gas. Known gap: a withdrawal confirmed and then
  reorged out keeps the Gas Fee posted at its confirmation (the design has no correction for it); requests
  that became final before this migration have none recorded.
- **Reaper** (task 3.9, invariant #17, `internal/pipeline/reaper.go`), one pass every `REAPER_INTERVAL`
  (30s). It frees stuck requests only when it can *prove* nothing can still reach the chain and no Hot
  Wallet nonce is left allocated but unused. Every write is a guarded `UPDATE ... WHERE status = <expected>`,
  so it never overwrites a row the request handler or pipeline just moved (it simply loses the race).

  | Status | Rule | Result |
  |---|---|---|
  | `requested` > `STUCK_REQUEST_TIMEOUT` (5 min) | nothing was ever held | `failed` / `stale_request`, no ledger call |
  | `validated` > `STUCK_RESERVATION_TIMEOUT` (5 min) | the key never reached wallet-service, so no nonce | Release, `failed` / `stale_reservation` |
  | `reserved` > 5 min | a sign call may have landed: only wallet-service's `void` rules that out | `voided` → Release, `failed` / `stale_reservation`; `already_signed` → `signed`; void fails or wallet-service is down → ALERT, retried next pass |
  | `signed` > 5 min | **never** Released on a timeout | ALERT naming the likely cause: underpriced, Hot Wallet lacks ETH, or an earlier nonce is unmined |
  | `signed`/`broadcast`: Hot Wallet's **mined** (`latest`) nonce > N **and** no receipt for any of the request's tx hashes (`broadcast`: also `STUCK_BROADCAST_BLOCKS` = 128 blocks) | nonce N is consumed by another tx, none of ours can land | Release, `failed` / `replaced_or_dropped` (this is how a Nonce Filler completes) |
  | `broadcast` > 128 blocks, N unconsumed | the tx may still be in a mempool | ALERT only |

  Notes: the mined nonce is read **before** the receipts (the other way round, our own tx mined between the
  two reads would look like another tx's), and the `pending` nonce is never used. ALERTs go to the log with
  an `ALERT:` prefix, each repeated at most every 10 minutes. The 128 blocks are counted from the chain head
  the Reaper first saw the row `broadcast` at (`broadcast_head_seen`), so it can only wait longer than 128
  blocks, never shorter. Stuck rows that are only ALERTed stay in the queue (500 per pass).
- **Destination checks** (task 3.10, invariant #18), on both `POST /v1/withdrawals` and
  `POST /v1/whitelist`:
  - Hot and Warm come from wallet-service's `GET /v1/wallet/system-addresses`, asked **once at
    startup** over mTLS as `CN=withdrawal-service` (30 tries, 4s apart, then exit). The listeners only
    start after that, so no request is ever served without them. Never an env var.
  - The deposit-address check is a live `GET /v1/addresses/{address}` on every request, no cache:
    `200` = ours → `422`, `404` = not ours → continue, anything else fails closed (`503`; a withdrawal
    stays `requested` and that key is spent).
  - Cold Wallet addresses are allowed (Decided).
- **Withdrawal Whitelist** (invariants #12, #13, #15, #18), mTLS, `CN=api-gateway` only:
  - `POST /v1/whitelist {user_id, address}` → `{id, user_id, address, status, added_at}`. A new entry is
    `201` and `pending`; re-adding the same address is `200` with the existing entry, and never restarts
    its cooldown. The address is stored in EIP-55 form, so two spellings are one entry. `400` for a
    malformed or zero address.
  - `status` is `active` once the Whitelist Cooldown (24h, decided) has passed since `added_at`. It is
    computed on read against the database clock; nothing flips it in the background.
    `WHITELIST_COOLDOWN` overrides it for e2e tests only (startup logs a warning).
  - `GET /v1/whitelist?user_id=` → `[{id, address, status, added_at}]`, the caller's own entries, newest
    first (`[]` when none). Lets a user spot a `pending` entry they did not add while the cooldown still
    protects them (approved by the Tech Lead; `withdrawal-flow.md` Open Questions #6).
  - `DELETE /v1/whitelist/{id}?user_id=` → `204`, immediate. Idempotent, and an unknown id or another
    user's entry is the same `204` no-op, so ids can't be probed.
  - `store.WhitelistStatus` is what validation (3.2) will use: only `active` lets a withdrawal through.
  - Creation runs the same destination checks (deposit / Hot / Warm → `422`, check unavailable →
    `503`), so a user doesn't wait out the cooldown for an entry that can never be used.

## Running it

```bash
make certs && make up   # Postgres (own database "withdrawal") + withdrawal-service, http://localhost:8086, mTLS :8446
                        # needs ASSETS_FILE / SCREENING_FILE (mounted), and reachable wallet-service,
                        # ledger-service (mTLS), RPC_URL and KAFKA_BROKERS: the whole stack runs together
                        # in e2e/ (`make -C ../../e2e up`)
make test               # store tests on a real Postgres (testcontainers or TEST_POSTGRES_DSN)
```

## Not built / to know

- In the `e2e/` stack (with `WHITELIST_COOLDOWN=2s`), but no e2e test drives it yet (task 3.8), and
  api-gateway does not route to it yet (task 3.7).
- `internal/screening` is a copy of ledger-service's loader (same file, same rules).
- Ran end to end on the `e2e/` stack by hand (deposit 2 ETH → whitelist → withdraw 0.5 ETH → reserved
  with 0.5 locked → confirmed, total 1.5, destination +0.5 ETH on-chain, event in Kafka and history-service).
  The automated e2e test is task 3.8.
- `internal/authz` is a copy of wallet-service's, as in ledger-service (a shared package needs a
  repo-root Docker build context).

## Metrics

`GET /metrics` on its own internal port (`METRICS_PORT`, default 9100; never on the API listeners). Scrape-time
collectors over this service's own database (`digiwal_collector_up{collector}` 0 and no series if a read fails):
`digiwal_withdrawals_total{status}` (requested / confirmed / failed / rejected, ever), `digiwal_withdrawal_amount_total{asset}`
(confirmed, whole units, display only), `digiwal_withdrawal_failures_total{status,reason}` (a closed set of reasons, anything
else is `other`), `digiwal_withdrawal_processing_seconds_sum` / `..._chain_wait_seconds_sum` / `..._timed_total` (our time vs
waiting for the chain: divide the rates for an average), `digiwal_withdrawal_requests{status}`, `digiwal_withdrawal_stuck{status}`,
`digiwal_withdrawal_oldest_age_seconds{status}`, `digiwal_nonce_gaps{state}`, `digiwal_whitelist_blocked`.
"Stuck" uses the very thresholds the Reaper is built with (`STUCK_REQUEST_TIMEOUT`, `STUCK_RESERVATION_TIMEOUT`,
`STUCK_BROADCAST_BLOCKS`), and a test compares it with the Reaper's own query at the boundary. `broadcast` stuck needs the chain
head, so it is a separate collector: an RPC failure drops that one series only. See `pkg/metrics`.
