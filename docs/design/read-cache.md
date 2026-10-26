# Read cache (cache-aside) — api-gateway auth + ledger balances

Status: Tech Lead decision recorded (cache-aside on existing Redis). Design only.

## Problem
- Every request pays two cross-WireGuard-link Postgres reads: API key -> user_id (api-gateway) and balances (ledger-service). Cache both in Redis to cut that latency/load.

## Contexts
- Identity/auth (api-gateway, `api_keys`) and Ledger (ledger-service, `accounts`). No new context, no new infra: Redis on vm3 is already used for rate limiting. Postgres stays the only source of truth.

## Decision (not re-argued): cache-aside
- Read: Redis GET -> hit returns; miss reads Postgres then SET with TTL.
- Write: write Postgres, commit, THEN `DEL` the key. Next read refills. Never write-through, never SET on the write path.

## Ground truth found in the code (corrections to the brief)
- The balances handler calls `BalanceSheet` (all assets, optional `asset_id` filter), not `BalancesFor`. `BalancesFor`/`Balance` have no non-test callers. Cache `BalanceSheet`.
- `Store.RevokeKey` (api-gateway `store.go`) is the only revoker (`UPDATE api_keys SET revoked_at`); no HTTP endpoint calls it. It must `DEL` after its UPDATE. Revocation done by hand in SQL bypasses the DEL, so only the TTL bounds it.
- `dev/loadtest` does not exist in this repo and no k6 script is committed (see Open questions).

## Cache 1: API key -> user_id (api-gateway, `Store.Authenticate`)
- Key: `dw:v1:apikey:<hex(HashKey(apiKey))>`. The raw key never enters Redis. Value: `user_id` string.
- TTL 5 min: a key's mapping changes only on revoke (rare, DEL covers it); TTL is only the safety net for out-of-band revocation. Flag: that is the max window a hand-revoked key still authenticates.
- Invalidate: `RevokeKey` after UPDATE commits. `CreateUser` does not touch the cache (new key, nothing stale).
- No negative caching: a miss on an unknown key must reach Postgres. Caching "invalid" would let a just-minted key (POST /users) be rejected until expiry, and lets an attacker fill Redis with junk keys.

## Cache 2: balances (ledger-service, `BalanceSheet`)
- Key: `dw:v1:bal:<user_id>`. Value: JSON of the full sheet (asset_id, symbol, total, locked, available as decimal strings: NUMERIC(78) overflows numbers). `asset_id` filter applied after the read, so one key per user to delete.
- TTL 5 s: balances change on every money event; the TTL is the stale bound, short enough that staleness stays a display matter.
- Write paths that change `total_balance`/`locked_balance`; each `DEL bal:<user_id>` AFTER `tx.Commit` succeeds (never before, never on rollback):
  - `Apply` (apply.go ~L306): deposits, corrections (user ids come from the derived postings, not the request), gas-fee entries. Delete for every user-kind account touched. Gas fees post to chain/`gas_expense` accounts only, so usually nothing to delete.
  - `Lock` (locks.go ~L119): raises `locked_balance`.
  - `Settle` (locks.go ~L205): lowers total and locked.
  - `Release` (locks.go ~L242): lowers locked.
  - Idempotent replays (no new commit) need no DEL. Any future writer to `accounts` must join this list; add a comment at each site.
- Money movement never reads the cache: Lock checks `total - locked >= amount` inside its DB transaction (and `Apply` checks the same in its UPDATE). The cache is display-only.

## Failure behaviour
- Redis error, timeout or down: treat as a miss and go to Postgres; log and count an error; never fail the request. A failed `DEL` is logged and counted; the TTL then bounds staleness.
- Same client settings as the rate limiter: 100 ms dial timeout, `MaxRetries: -1`, plus 100 ms read/write timeouts (add; the limiter sets none). `REDIS_PASSWORD` from env. ledger-service needs `REDIS_ADDR`/`REDIS_PASSWORD` added to its deploy config (new dependency for that service, same Redis).

## Known race (stated plainly)
- Reader misses and reads old DB value; writer commits and DELs; reader then SETs the old value. Stale entry survives until TTL.
- Bound: at most 5 s for balances, 5 min for API keys (API key race needs a revoke concurrent with a miss, so it is rare). No version/lease scheme (not needed for a display cache; revisit only if the bound is rejected).

## Metrics (pkg/metrics, exposed on each service's :9100 /metrics)
- `cache_requests_total{cache="apikey|balances",result="hit|miss|error"}` and `cache_invalidations_total{cache,result="ok|error"}`. Hit ratio = hit / (hit+miss). Label values are fixed (no user ids; `CheckLabels` applies).

## Tests required
- Unit (miniredis or fake): hit, miss+refill, Redis error falls through to DB, TTL set, flag off bypasses Redis.
- Invalidation: each of Apply/Lock/Settle/Release/RevokeKey deletes after commit; NOT deleted when the tx rolls back or the commit fails; replayed idempotent call is harmless.
- Revoked key rejected right after `RevokeKey` while cached.
- Balances: Lock after a cached read still honours the DB balance (cache cannot cause overdraft).
- Redis slow (>100 ms) still returns correct answer in bounded time.
- Tester: re-run the ledger invariant checks (auditor L2/L3) after a load run with cache on.

## Rollback
- Env flag per service: `AUTH_CACHE_ENABLED` (api-gateway), `BALANCE_CACHE_ENABLED` (ledger-service). Default 0 (off): the first deploy changes nothing, we flip on per VM and compare, and a missing env var in any environment is the safe state. Turning off needs only a restart; stale keys expire on their own (<= 5 min). Flip to 1 in vm configs once measured.

## Measurement
- Run the existing k6 balances scenario against the same stack, flag 0 then 1: p50/p95/p99 and RPS of `GET /v1/balances` and ledger Postgres query rate; read hit ratio from the metrics above (expect high for apikey, workload-dependent for balances). Record in a short results note.

## Alternatives rejected
- In-process cache (no Redis): free of the network, but per-instance, so a DEL on one node misses others on a multi-node setup.
- Write-through / SET on write: concurrent writers can store out-of-order values; DEL is the decision made.

## Open questions for the Tech Lead
1. k6 balances scenario: the script is not in the repo (`dev/loadtest` absent). Where does it live, or should the Dev commit it?
2. Is a 5 min window of a hand-revoked key still authenticating acceptable? Otherwise lower the TTL or add a revoke endpoint/CLI that calls `RevokeKey`.
3. Default flag off (proposed) vs on?
4. Deploy: ledger-service gets Redis access (vm3 password) for the first time; confirm firewall/WireGuard path from the ledger node.

## What this does NOT fix
- Write-path latency (Lock/Settle/Apply/POST withdrawals still pay the DB and link cost) and the cache-miss path; it only speeds repeat reads.
