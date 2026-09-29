# ledger-service

Source of truth for balances (`docs/design/ledger.md`): double-entry accounts and
immutable entries in Postgres, plus the Asset Registry mirror. Everything that changes a
balance goes through one operation, `Apply`.

## What works

- **Balances** are Total (settled), Locked (held for in-flight withdrawals) and
  Available = Total - Locked (`ledger.md`). All amounts are `NUMERIC(78,0)`.
- **Lock / Settle / Release** for withdrawals, all keyed on the Withdrawal Request's
  idempotency key:
  - `POST /v1/ledger/locks` `{idempotency_key, user_id, asset_id, amount}`: reserves capacity, posts
    no Entry. `422 insufficient_funds` if Available can't cover it. The check and the hold are one
    UPDATE, so concurrent locks serialize on the account row and can't over-reserve. A replay
    returns the lock's current status; same key with a different request is `409`.
  - `POST /v1/ledger/locks/{key}/settle`: the real debit, one atomic transaction (lock flip, both
    balance updates, both entries). Never routed through `Apply`. Replay returns the same result.
  - `POST /v1/ledger/locks/{key}/release`: gives the capacity back, posts nothing.
  - Settled xor released, exactly once. A conflicting resolution is **`409`, never `200`**, so the
    caller can't mistake a released lock for a debited one.
- **`POST /v1/ledger/transactions`** posts a **correction** only: it names the entries to reverse
  (`corrects_entry_id`); the account and amount are derived server-side as their exact negation and
  whatever the caller sends for them is ignored. Every leg must be reversed, and an entry can be
  reversed once. A correction's debit is checked against Available, so it can't undercut an active Lock.
- **Deposits** enter only through the `deposits.confirmed` consumer (never over REST, so they can't
  skip screening). `Apply` is idempotent on `(chain_id, tx_hash, log_index)`; sum-to-zero per asset.
- All four endpoints are on the **mTLS** listener (`:8443`), `CN=withdrawal-service` only
  (`service-auth-mtls.md` invariant #7).
- **Asset Registry**: the one canonical `infra/assets.yaml`, validated strictly at startup, synced
  into Postgres; assets that leave the file are deactivated, never deleted.
- **Screening List** (`infra/screening.yaml`) and the consumer: flag (never credit) a sender on
  the list, otherwise credit. Events that can never succeed go to `failed_events` and are
  acknowledged; transient failures are retried, and the offset is committed only after success.

## Running it

```bash
make certs   # CA + leaf certs (shared with wallet-service)
make up      # Postgres + ledger-service (own database "ledger"), http://localhost:8081
make test    # integration tests need Postgres: testcontainers (Docker) or TEST_POSTGRES_DSN
```

## Open gaps

- **Deposit consumer is off: there is no `UserResolver`.** `deposits.confirmed` carries
  `to_address` but no `user_id`, and no design doc says how the ledger learns which user owns an
  address (only wallet-service knows). See `consumer.UserResolver`.
- **A correction names `corrects_entry_id`, but `withdrawal-service` can't know entry ids.** Settle
  returns a `ledger_transaction_id`, not entry ids. The reference should probably be the
  transaction or the lock key.
- Settling a released lock (and vice versa) returns `409`; the docs say "ok (no-op)" for any
  already-resolved lock. Only the same resolution is treated as a no-op here.
- Every deposit/settle of an asset updates that asset's single Chain Account row, so they
  serialize on it (a throughput ceiling to look at in the load-test phase).
- No dead-letter design exists; `failed_events` is a stopgap so nothing is dropped silently.
- `internal/authz` and `internal/testdb` are copies of wallet-service's; a shared package needs a
  repo-root build context (this service already builds from the root, for `pkg/events`).
