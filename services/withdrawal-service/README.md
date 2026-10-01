# withdrawal-service

Orchestrates withdrawals (`docs/design/withdrawal-flow.md`). **Tasks 3.1-3.3, 3.6, 3.10 and the
withdrawal-service part of 3.11 so far:** a request is recorded idempotently, validated, Reserved on the
ledger, then signed, broadcast, confirmed and Settled in the background. Nonce Gap handling (3.4),
refunds (3.5) and the Reaper (3.9) are not built yet.

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
     allocates a second nonce. A refusal (400/409/422: no nonce allocated) Releases the Lock and fails the
     request (`sign_refused`); anything else (503 during a Nonce Gap, 5xx, timeout) is retried.
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
