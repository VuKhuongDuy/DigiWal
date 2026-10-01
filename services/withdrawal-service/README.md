# withdrawal-service

Orchestrates withdrawals (`docs/design/withdrawal-flow.md`). **Tasks 3.1 and 3.6 so far:** the service,
its schema, idempotent request creation, and the Withdrawal Whitelist. A request is recorded and stays
`requested`; validation (3.2), Reservation/signing/broadcast/Settle (3.3), Nonce Gap handling (3.4), the
destination checks (3.10) and the Reaper (3.9) are not built yet.

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
    (invariant #11). New: `202 {idempotency_key, status, tx_hash}`.
  - Duplicate (same user, same key, same request): `200` with the existing row's status. It never
    re-validates, whatever the status (Tech Lead, Open Questions #9b option B).
  - Same user and key but a **different** request (asset, amount or address): `409`. This is not a
    retry; answering with another request's status would mislead the client. Not spelled out in the
    design docs (which only cover the identical-retry case); it matches ledger-service's and
    wallet-service's handling of a reused key.
  - `400` only for a body that cannot be stored (bad JSON, unknown field, missing field, a key that is
    not 1-128 printable ASCII characters, an amount that is not a positive integer in minor units).
    Everything else — asset registered, minimum, address format, whitelist, screening, destination —
    is task 3.2's validation and ends in `rejected`.
- **`GET /v1/withdrawals/{idempotency_key}?user_id=`** (mTLS, `CN=api-gateway`): the caller's own
  request, `{idempotency_key, status, tx_hash, reject_reason?}`. Another user's key is `404`, the same
  as a missing one.
- `withdrawal_id` and `user_id` are never returned.
- **Withdrawal Whitelist** (invariants #12, #13, #15), mTLS, `CN=api-gateway` only:
  - `POST /v1/whitelist {user_id, address}` → `{id, user_id, address, status, added_at}`. A new entry is
    `201` and `pending`; re-adding the same address is `200` with the existing entry, and never restarts
    its cooldown. The address is stored in EIP-55 form, so two spellings are one entry. `400` for a
    malformed or zero address.
  - `status` is `active` once the Whitelist Cooldown (24h, decided) has passed since `added_at`. It is
    computed on read against the database clock; nothing flips it in the background.
    `WHITELIST_COOLDOWN` overrides it for e2e tests only (startup logs a warning).
  - `DELETE /v1/whitelist/{id}?user_id=` → `204`, immediate. Idempotent, and an unknown id or another
    user's entry is the same `204` no-op, so ids can't be probed.
  - `store.WhitelistStatus` is what validation (3.2) will use: only `active` lets a withdrawal through.
  - The invariant #18 check at creation (deposit / Hot / Warm address → `422`, check unavailable → `503`)
    is a hook, `api.Destinations`, unset until task 3.10 wires wallet-service.

## Running it

```bash
make certs && make up   # Postgres (own database "withdrawal") + withdrawal-service, http://localhost:8086, mTLS :8446
make test               # store tests on a real Postgres (testcontainers or TEST_POSTGRES_DSN)
```

## Not built / to know

- Not in `e2e/` yet, and api-gateway does not route to it yet (task 3.7).
- No endpoint lists a user's whitelist entries: none is designed. Without one a user cannot see a
  `pending` entry an attacker added (`withdrawal-flow.md` Open Questions #6).
- No `pkg/events` dependency yet: the withdrawal events come with task 3.3, keyed on `withdrawal_id`
  (task 3.11).
- `internal/authz` is a copy of wallet-service's, as in ledger-service (a shared package needs a
  repo-root Docker build context).
