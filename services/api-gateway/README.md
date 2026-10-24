# api-gateway

The only service meant to be reachable from outside the deployment. Its jobs today are Identity
(`docs/design/identity-auth.md`, task 0.7): no request reaches another service without a `user_id`
resolved from a valid API key, and that `user_id` is never read from the client.

## What works

- `POST /users` — mints a system-generated `user_id` (UUID) and an API key, returned **once**:
  `201 {"user_id","api_key"}` (`Cache-Control: no-store`). The body is ignored; a client cannot
  choose its id. The key is `dw_` + 64 hex chars (32 random bytes); Postgres stores only its SHA-256.
- `Authorization: Bearer <api_key>` on **every other route, including paths that do not exist**:
  missing/wrong/unknown/revoked -> `401` before anything downstream is called. A failed *lookup*
  (database down) is `503`, never `401`.
- Revocation is a live lookup (`revoked_at IS NULL`), so it applies to the very next request.
  There is no HTTP endpoint to revoke (none is designed); `store.RevokeKey` is for an operator tool.
- `GET /v1/deposit-address` (task 2.5) — the authenticated user's own address. Always calls
  wallet-service's `POST /v1/users/{id}/deposit-address` (idempotent get-or-create), never `GET`, so a
  first-time user never sees a 404 (decided in `integration-contracts.md` §2). The id comes only from the key.
- `GET /v1/balances[?asset_id=]` (task 2.5) — `[{asset_id, symbol, total_balance, locked_balance,
  available_balance}]`, one row per registered asset (zeros for untouched ones), from ledger-service's
  `GET /v1/ledger/balances/{user_id}` over mTLS as `CN=api-gateway`. Query params, headers and body that
  name another user are ignored (tested); only the asset filter passes through.

## Not built / to know

- The gateway -> wallet-service hop is plain HTTP: `service-auth-mtls.md` Open Question #4 (moving
  those endpoints behind mTLS, `CN=api-gateway`) is deferred.
- Rate limiting is approximate (sliding-window counter, two counters per key): `RATE_LIMIT_IP_PER_MIN`
  (default 1200; covers `POST /users` and bad-key guessing, keyed by `RemoteAddr`, so behind a proxy it sees
  the proxy) and `RATE_LIMIT_USER_PER_MIN` (default 600, per authenticated user); 0 disables. Over the limit ->
  429 + `Retry-After`. Without `REDIS_ADDR` it is in-memory and per instance (N instances allow up to N× the
  limit). With `REDIS_ADDR` the budget is shared via one Lua call per request (50 ms timeout); on any Redis
  error the gateway **fails open** to the in-memory limiter and skips Redis for 1 s, so an outage costs
  cross-instance accuracy, not availability. `REDIS_PASSWORD` (optional) is sent to Redis. In the two-node layout Redis on vm3 requires a
  256-bit password from `make env` and is published only on 10.10.0.2 (WireGuard); vm2's gateway uses it
  (`make env` on vm2 copies the password from vm3 over ssh, so run it on vm3 first) and falls back to
  in-memory limits if the link is down. No lockout or backoff on repeated bad keys beyond the IP limit.
- TLS termination for external clients is out of scope here (plain `:8080`).
- Runs under Docker in `e2e/` (task 2.6) against the real wallet-service and ledger-service, including the
  gateway -> ledger-service mTLS balances call. Its own tests use real Postgres and fake wallet-service / ledger-service.

## Running it

```bash
make certs && make up   # Postgres (own database "api_gateway") + api-gateway, http://localhost:8084
make test    # Postgres via testcontainers or TEST_POSTGRES_DSN
curl -s -XPOST localhost:8084/users -d '{}'                          # {"user_id":...,"api_key":...}
curl -s -H "Authorization: Bearer $KEY" localhost:8084/v1/deposit-address
```

## Metrics

`GET /metrics` on its own internal port (`METRICS_PORT`, default 9100; not published, **never** on the public
listener, which has a test for that). One scrape-time collector reading this service's own database:
`digiwal_users_total` (users ever created; monotonic, so new users per day is `increase(...[1d])`) and
`digiwal_collector_up{collector="api-gateway"}`. If the database cannot be read the series is omitted, not zero.
See `pkg/metrics` and `docs/design/monitoring-dashboard.md`.

**Performance metrics.** Both API listeners are measured server-side: `digiwal_http_request_duration_seconds{service="api-gateway",route,code}`
(route = the ServeMux pattern, never the raw path) and the database pool's saturation, `digiwal_db_pool_*` (see `pkg/metrics`).
Its authenticated routes sit in a mux nested behind the authentication middleware, so that mux is wrapped with `metrics.ReportRoute`
(otherwise every authenticated request would be labelled with the root's catch-all `/`).
