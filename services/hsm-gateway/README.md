# hsm-gateway

Fronts the HSM Token (SoftHSMv2 via PKCS#11) over mTLS. It signs **Hot Wallet** transactions from a
*Sign Intent*, never from a digest, under a static **Signing Policy**, and writes a hash-chained **Audit
Log** that is fsynced before any signature leaves. It holds the Hot Key (secp256k1, generated inside the
token: `CKA_PRIVATE=true`, `CKA_SENSITIVE=true`, `CKA_EXTRACTABLE=false`) and nothing else: no database, no
software key path, no seed, no KEK. Design: `../../docs/design/key-custody-hsm.md` (M2a); spike results:
`../../docs/design/key-custody-hsm-m0-results.md`. `wallet-service` signs the Hot Wallet through it with
`SIGNER_BACKEND=pkcs11` (M2b); the dev wiring is `e2e/docker-compose.hsm.yml`, `dev/nodes/vm2.yml` and `make hsm-setup`
(`dev/`, `e2e/`). The Hot cutover (draining the old Hot address) is M2c and not done.

## API (mTLS listener, `MTLS_ADDR`, default `:8443`)

Callers need a certificate that chains to the project CA **and** whose leaf SHA-256 is pinned in
`CALLERS_FILE` (K15), and whose CN is on the endpoint's allow-list (`internal/authz`). Anything else: handshake
failure or an opaque `403 {"error":"forbidden"}`.

```
POST /v1/hot/sign     CN: wallet-service
GET  /v1/hot/key      CN: wallet-service, sweep-signer
GET  /v1/audit/head   CN: auditor, key-admin           -> {seq, hash}; the auditor stores it (tail-truncation detection)

# plain listener :8080 (PORT)            GET /healthz   GET /readyz (503 while signing is closed)
# metrics :9100 (METRICS_PORT)           GET /metrics
```

`POST /v1/hot/sign` body (unknown fields are rejected, so there is no `digest`/`hash`/`signature` field to send):

```json
{"chain_id":31337,"nonce":7,"to":"0x..","value":"1000","data":"0x","gas":21000,
 "max_fee_per_gas":"2000000000","max_priority_fee_per_gas":"1000000000",
 "operation":"payout","idempotency_key":"k1","caller_cn":"wallet-service"}
```

`value` and fees are decimal strings (wei), `data` is hex, `operation` is `payout | treasury_sweep | cancel`
(a replacement is detected by a nonce the gateway has already signed, not claimed). `caller_cn` is a claim that is
audited next to the verified certificate CN and never trusted. The transaction is always EIP-1559, empty access
list, recipient required.

| Status | Body | Meaning |
|---|---|---|
| 200 | `{signed_raw_tx, tx_hash, from, audit_seq, policy_hash}` | signed; the authorizing audit line is already durable |
| 400 | `{error:"bad_request", detail}` | malformed / unknown field / no recipient |
| 403 | `{error:"policy_refused", rule}` | Signing Policy refusal, rule id below; no signature, an ALERT log line |
| 503 | `{error}` = `hsm_unavailable`, `hsm_not_logged_in`, `hsm_pin_rejected`, `hsm_key_missing`, `hsm_key_attributes`, `hsm_bad_output`, `hsm_internal`, `audit_unavailable`, `signature_verification_failed`, `not_serving` | fail closed; never a software fallback |

`GET /v1/hot/key` -> `{address, public_key, chain_id, policy_hash}`. Nothing returns private key material.

## Signing Policy (`POLICY_FILE`, see `config/policy.example.yaml`)

One root-owned, read-only file (refused if group/world-writable or, by default, not owned by uid 0:
`POLICY_REQUIRE_ROOT_OWNER=false` relaxes only the owner check, for dev). Its SHA-256 is logged at start and is in
every audit entry. No hot reload: edit and restart. Every cap is required (no "unlimited" default).

Rule ids (`internal/policy/engine.go`): `kill_switch`, `rate_limit`, `chain_pin`, `reserved_nonce`,
`unknown_operation`, `data_malformed`, `erc20_approve_refused`, `data_selector_refused`, `erc20_value_nonzero`,
`asset_not_allowed`, `recipient_zero|hot|token_contract|denied`, `class_mismatch`, `treasury_not_configured`,
`cancel_shape`, `gas_cap`, `fee_ceiling`, `tip_above_max_fee`, `max_per_tx`, `treasury_max_per_tx`, `window_1h`,
`window_24h`, `replacement_not_bump`. Data is empty (native) or exactly `transfer(address,uint256)`; `approve` and
everything else are refused. Treasury Sweeps (recipient = the pinned Warm address) have their own cap and do not
consume the Policy Windows; Cancels are zero-value to Hot at any nonce; a signed nonce may be signed again only as
the same body at equal-or-higher fees (a retry or a Fee Bump) or as a Cancel; windows count each nonce once at its
maximum and are rebuilt from the audit log at start. Worst-case fees (`gas * max_fee`) count in the native window.

## Audit Log (`AUDIT_LOG_FILE`)

JSON lines, each carrying `prev` (hash of the previous line) and `hash`. Appended and `fsync`ed before the HSM is called;
if the append fails there is no signature. A torn final line (crash mid-append, never fsynced) is trimmed at start; any
other break refuses to start. Set `chattr +a` on the file on the host.

```
service audit-verify -file audit.log [-expect-seq N -expect-hash H]   # exit 1 on tamper/removal; -expect-* catch tail truncation
```

## HSM and secrets

* The PIN is read from `HSM_PIN_FILE` only (never env/flags, never logged). Login happens once per process; a rejected
  PIN exits with code 3 and is recorded in `PIN_GUARD_FILE`; after 3 failures within an hour the gateway refuses to try the
  token at all until an operator investigates and deletes the file (SoftHSM never locks on wrong PINs, M0 (e)).
* A session pool of 4: a session handle is in the free channel or in exactly one lease, never shared (a shared session
  SIGSEGVs libsofthsm2, M0 (b)).
* At start, on re-open and every `SELFTEST_INTERVAL` (10 m) the gateway reads the key attributes back (must be private,
  sensitive, never extractable, EC/secp256k1) and signs a transaction that can never be mined (nonce 2^64-1), recovering
  it against the pinned Hot address in low-s form. Start-up mismatch exits; a later mismatch closes `/v1/hot/sign` until
  restart, an HSM error closes it until the next pass.

## Ceremony and operation

```
make certs                        # leaf cert for CN hsm-gateway (project CA stays outside the repo)
make secrets                      # data dirs + a random user PIN file (outside git); chown 10001 and chmod 0400 on the host
read -rs SO && printf '%s\n' "$SO" | make init-token   # SO PIN typed once, never stored; prints the Hot address
cp config/policy.example.yaml config/policy.yaml        # put the Hot address in it, tune the caps
hsm-gateway fingerprint ../../infra/pki/out/wallet-service/cert.pem   # -> config/callers.yaml
docker compose up -d hsm-gateway
```

## Tests

```
make test            # unit tests, -race, no HSM (a software stand-in exists only in _test files)
make test-softhsm    # the whole suite incl. -tags softhsm against a real SoftHSMv2 in Docker (host has none)
```


## xpub Attestation (M3a)

`POST /v1/attest` (caller CN `key-admin`, pinned leaf) signs the canonical statement over (chain, path, xpub) with a dedicated
attestation key, a P-256 key generated inside the token (`CKA_PRIVATE=true`, sensitive, non-extractable; a different key from the Hot
Key, and the Hot path cannot reach it). `GET /v1/attest/key` (key-admin, auditor) returns its public key. The body is exactly
`{"chain":"evm","path":"m/44'/60'/0'","xpub":"xpub..."}`: anything else (another chain or path, a private key, a key not at depth 3 with hardened
child 0, extra fields such as a digest) answers `422 attest_refused`, reaches no token and is audited (`op: attest`, `result: refused`). Every
accepted call is appended and fsynced before the token is asked, like a sign call. The signed bytes are
`digiwal/xpub-attest/v1\nchain=evm\npath=m/44'/60'/0'\nxpub=<xpub>\n`, signature ECDSA P-256 over SHA-256 of them (raw 64-byte r||s).
`init-token` generates both keys and prints `attest_public_key=<hex>`; for a token from before M3a run `hsm-gateway init-attest-key`. A token
without the key still serves Hot signing (`/v1/attest` answers 503). The public key is pinned in wallet-service's `XPUB_ATTEST_PUBKEY_FILE`
(`docs/runbooks/xpub-migration.md`).

## Vault: Sealed Secrets under a KEK in the HSM (M3c)

The token holds a third key besides the Hot Key and the attestation key: the **KEK** (`kek-v1`, `kek-v2`, ...), AES-256, generated inside the token with `CKA_PRIVATE=true` (spike M0 (d)),
sensitive, non-extractable, wrap/unwrap only (checked at start and on every use; a KEK that fails its checks stops the gateway). `init-token` makes `kek-v1`; a token from before M3c gets it with
`hsm-gateway init-kek` (then restart). The gateway never stores a secret: callers keep the rows (`vault.sealed_secrets`).

| Route (mTLS, pinned leaf) | CN | Does |
|---|---|---|
| `POST /v1/vault/seal` `{role, purpose, version, kek_version?, plaintext}` | key-admin | DEK generated and wrapped (`CKM_AES_KEY_WRAP`) **inside the token**, plaintext encrypted with `CKM_AES_GCM` inside the token under a **gateway-generated random 96-bit nonce** (the token accepts a zero IV: this code refuses any nonce that is not 12 non-zero bytes), AAD = `role\|purpose\|version\|kek_version\|hex(SHA256(wrapped_dek))`. Returns `wrapped_dek, nonce, ciphertext, aad, kek_version`; the DEK object is destroyed |
| `POST /v1/vault/unseal` (the stored row) | **sweep-signer only** (and only for roles whose table entry names it) | rebuilds the AAD from the row's own fields and compares, rate cap (`VAULT_UNSEAL_PER_MIN`, default 60), **audit line fsynced before the token is asked**, unwrap + GCM-decrypt inside the token; the answer is the raw plaintext as `application/octet-stream`, nothing else. Any tamper or AAD mismatch: `422 authentication_failed` (SoftHSM's `CKR_GENERAL_ERROR` is mapped to it explicitly) |
| `POST /v1/vault/verify` (a row) | key-admin | opens the row inside the gateway and returns only the PUBLIC account xpub of the seed, so rotations are verified without the seed leaving |
| `POST /v1/vault/reseal` `{row, to_version, to_kek_version?, new_dek}` | key-admin, **Maintenance Window** | decrypts and re-encrypts inside the gateway/token under a new row version, a new nonce, the new KEK and (rotate-dek) a new DEK; the caller never sees a plaintext |
| `POST /v1/vault/kek/{generate,retire,destroy}` `{version}` | key-admin, **Maintenance Window** | consecutive versions only, idempotent generate; retire = `CKA_WRAP=false` (unwrap-only); destroy needs retired and not newest |
| `GET /v1/vault/keks` | key-admin, auditor | versions and wrap flags |

Every seal, unseal, verify, reseal and KEK call is an Audit Log line (`class: vault`, role, purpose, row version, KEK version, a short fingerprint of the wrapped DEK, never the plaintext, DEK or ciphertext);
refusals are audited too and counted in `digiwal_hsm_gateway_vault_total{result}`. KEK object creation/destruction pauses the session pool for a few milliseconds (found by the rotation-under-load test: SoftHSM
can fail a token-object create while other sessions run operations).

**Maintenance Window**: `hsm-gateway maintenance open 15m | close | status` writes `MAINTENANCE_FILE` (default `/var/lib/hsm-gateway/state/maintenance-window`, capped at 1 h) on the gateway host;
the admin routes read it per call. A stolen key-admin certificate alone cannot open one. Roles and purposes are a hardcoded table (`internal/vault`): today only `user_derivation` / `sweep` (64 bytes).

Tests: `internal/vault` (AAD, strict decoding), `internal/gateway` and `internal/server` (a software token stand-in: CN allow-lists, pinning, audit-before-return, rate limit, window), and in the SoftHSM image
(`make test-softhsm`) the real token: round trip, 9 tamper classes, 1500 unique nonces and zero-IV refusal, rewrap/rotate-dek, KEK rotation with 12 goroutines unsealing, retire/destroy guards, `init-kek` upgrade, a non-private KEK refused.
