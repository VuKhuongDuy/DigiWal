# api-gateway

The only service meant to be reachable from outside the deployment. Its job today is Identity
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
- `GET /v1/deposit-address` (task 2.5) — the authenticated user's own address, from `wallet-service`
  (`GET /v1/users/{id}/deposit-address`, as documented). The id comes only from the key: query
  params, headers and body that name another user are ignored (tested).

## Not built / to know

- **`GET /v1/balances` is not built.** The gateway must call `ledger-service`, which has **no
  endpoint that reads a user's balances** (`integration-contracts.md` §2's ledger table lists none,
  and no CN is allowed for one). Needs a design entry: path, request/response, `CN=api-gateway`.
- **`GET /v1/deposit-address` also issues the address on first use.** The doc only lists the `GET`,
  but for a new user that is a 404 forever and nothing else creates the first address. The gateway
  falls back to wallet-service's idempotent `POST` after a 404. Needs the design to say who issues.
- The gateway -> wallet-service hop is plain HTTP: `service-auth-mtls.md` Open Question #4 (moving
  those endpoints behind mTLS, `CN=api-gateway`) is deferred.
- No rate limiting or brute-force protection on `POST /users` or on bad keys (`identity-auth.md`
  Open Question #3, deliberately deferred): anyone can mint users today.
- TLS termination for external clients is out of scope here (plain `:8080`).
- Not run under Docker (none in the sandbox); tests use real Postgres and a fake wallet-service.

## Running it

```bash
make up      # Postgres (own database "api_gateway") + api-gateway, http://localhost:8084
make test    # Postgres via testcontainers or TEST_POSTGRES_DSN
curl -s -XPOST localhost:8084/users -d '{}'                          # {"user_id":...,"api_key":...}
curl -s -H "Authorization: Bearer $KEY" localhost:8084/v1/deposit-address
```
