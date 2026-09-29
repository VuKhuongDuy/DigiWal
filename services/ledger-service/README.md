# ledger-service

Source of truth for balances (`docs/design/ledger.md`): double-entry accounts and
immutable entries in Postgres, plus the Asset Registry mirror. Everything that changes a
balance goes through one operation, `Apply`.

## What works

- **`Apply`** — atomic, idempotent, sum-to-zero (per asset), overdraft-proof. One DB
  transaction; the row lock of `UPDATE accounts ... WHERE balance + x >= 0` is the sole
  correctness boundary against overdraft. Same key + same content = `already_applied`;
  same key + different content = `409`.
- **`POST /v1/ledger/transactions`** on the **mTLS** listener (`:8443`), client cert
  `CN=withdrawal-service` only (`service-auth-mtls.md` invariant #7). Only
  `source_type` `withdrawal` / `correction`: deposits enter through the screened consumer,
  never this endpoint. `422 {"status":"insufficient_funds"}` on overdraft.
- **Asset Registry**: loads the one canonical `infra/assets.yaml`, validated strictly at
  startup (a bad entry fails boot), synced into Postgres; assets that leave the file are
  deactivated, never deleted.
- **Screening List** (`infra/screening.yaml`) and the `deposits.confirmed` consumer:
  screens the sender, flags (never credits) a match, otherwise credits the user. Events that can
  never succeed go to `failed_events` and are acknowledged; transient failures are retried
  and the offset is committed only after success.

## Running it

```bash
make certs   # CA + leaf certs (shared with wallet-service)
make up      # Postgres + ledger-service (own database "ledger"), http://localhost:8081
make test    # integration tests need Postgres: testcontainers (Docker) or TEST_POSTGRES_DSN
```

## Deviations from ledger.md, and open gaps

- **Amounts are `NUMERIC(78,0)`, not `bigint`.** int64 tops out near 9.2e18, i.e. about
  9.2 ETH in wei, so a `bigint` balance overflows on a modest deposit. `NUMERIC(78,0)` holds any uint256.
- **The Chain Account is exempt from the no-negative rule** (it is negative by
  construction: `-(net funds users hold)`). ledger.md's invariant #1 doesn't say so.
- **Sum-to-zero is checked per asset**, so +1 of one asset can't be balanced by -1 of another.
- **Deposit consumer is off: there is no `UserResolver`.** `deposits.confirmed` carries
  `to_address` but no `user_id`, and no design doc says how the ledger learns which user owns
  an address (only wallet-service knows). See `consumer.UserResolver`.
- `correction` lets withdrawal-service credit any user; it is how a refund (Compensating Entry)
  is posted. A compromised withdrawal-service can therefore mint balances. Consider requiring a
  correction to reference the original transaction.
- Every deposit/withdrawal of an asset updates that asset's single Chain Account row, so they
  serialize on it (a throughput ceiling to look at in the load-test phase).
- No dead-letter design exists; `failed_events` is a stopgap so nothing is dropped silently.
- `internal/authz` and `internal/testdb` are copies of wallet-service's; a shared package needs
  a repo-root build context (this service already builds from the root, for `pkg/events`).
