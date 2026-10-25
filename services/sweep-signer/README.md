# sweep-signer

The **only** place the `user_derivation` Seed exists in plaintext (key-custody-hsm.md M3b, K10). It signs **Sweeps** (a user's derived key ->
the Hot Wallet) for `sweeper` and nothing else. `wallet-service` has no seed and no user-key endpoint any more. Design:
`../../docs/design/key-custody-hsm.md` (invariants 14-17, 5.4); spike results `../../docs/design/key-custody-hsm-m0-results.md` (g).

## API

mTLS only (`MTLS_PORT`, default `:8443`): the client leaf must chain to the project CA **and** be the exact leaf pinned in `CALLERS_FILE`
(SHA-256 of the DER, `infra/pki/gen-callers.sh <file> sweeper`) **and** its CN must be `sweeper`. Anything else: handshake failure or an opaque
`403`. No other operation exists.

```
POST /v1/sign/user-address   {user_id, to_address, asset_id, amount, chain_id} -> {signed_raw_tx}      # the contract sweeper has always used
# plain :8080   GET /healthz (liveness only)         metrics :9100   GET /metrics        (neither is published)
```

| Status | When |
|---|---|
| 200 | signed |
| 400 | malformed body or amount, unknown asset, `chain_id` is not the pinned chain |
| 403 | `to_address` is not the pinned Hot address; the derived address is not the issued one; the tx would not be a plain sweep |
| 404 | the user has no issued address (no index was ever issued) |
| 422 | native `"max"` with a balance that does not cover the gas |
| 503 | the Address Audit has not passed or found a mismatch; the fee cap is above the Fee Ceiling; the Seed is not available |

## What it refuses (invariant 15), each counted in `sweep_signer_refusals_total{rule}` and logged as `ALERT`

`destination` (must equal the Hot address pinned from `hsm-gateway` `GET /v1/hot/key` at start, never from the request), `chain` (the pinned chain, which
must equal the RPC's), `asset` (registered in the Asset Registry; native transfer or ERC-20 `transfer(hot, amount)` only), `index` (the user must have an
issued `wallet_addresses` row: the mapping user -> derivation index comes from the database), `fee_ceiling` (`SWEEP_MAX_FEE_PER_GAS_WEI`, default 50 gwei),
`address_mismatch` (the key derived at that index must be the stored address: a swapped xpub is refused here and freezes everything), `audit_frozen`,
`tx_shape`. The existing sweep rules are carried over unchanged: live balance for `max`, the fee **cap** reserved, the live pending nonce.

## The Seed in memory (invariant 14), and what that does and does not give

Per request (M3c): read the ACTIVE Sealed Secret row (`vault.active_secrets`: ciphertext, wrapped DEK, nonce, AAD), send it to `hsm-gateway` `POST /v1/vault/unseal` (mTLS, this
service's pinned leaf, CN `sweep-signer`: the only caller allowed), which unwraps the DEK with the KEK **inside the HSM**, decrypts inside the HSM and answers with the 64 raw bytes
(`application/octet-stream`), read **directly into one dedicated page** (anonymous `mmap`, `mlock` where `RLIMIT_MEMLOCK` allows,
`MADV_DONTDUMP`), derive `m/44'/60'/0'/0/{index}` with btcd `hdkeychain` on `[]byte`, sign with `btcec` (`SignCompact`, RFC 6979, low-s; the Ethereum
`(r, s, v)` is assembled by hand), then **zero every derived node, the `*btcec.PrivateKey` and the page** and unmap it, on every path including errors
and panics. No `string` ever holds the Seed, there is no mnemonic and no BIP39/go-ethereum-hdwallet code in the binary (a test pins that through `go list -deps`).
Honest limits: Go can copy stacks, hdkeychain keeps HMAC temporaries it does not expose, and the TLS record buffers that carried the seed from
hsm-gateway are Go heap memory this process cannot wipe: zeroing is **best effort**. The real controls are isolation (the container below), a one-request lifetime, no
swap, no core dump and a tiny process. This service holds **no key of any kind** (no master key, no KEK, no DEK). If `hsm-gateway` is down, rate-limits, or answers "authentication failed" (a tampered row), there is
no Seed and so no signature (`503`), and the Address Audit pass is not a verdict; nothing falls back.

## Address Audit (invariant 11)

A loop (`AUDIT_INTERVAL`, default 30 s; 5 s in the dev stacks) opens the Seed once per pass, re-derives every address issued since the last pass plus a
rolling window of older ones and compares with `wallet_addresses`, and compares the Seed's account xpub with `wallet_xpubs`. **One mismatch freezes all
signing until restart**, logs `ALERT` and sets `sweep_signer_signing_frozen`. Until the first pass is clean nothing is signed (`sweep_signer_ready`). A
pass that fails for infrastructure reasons is not a verdict: it neither freezes nor opens the gate. A row that commits out of order behind the watermark
is caught by the rolling window.

## Hot address source

`HOT_SOURCE=gateway` (default; the only one in a `TAGS=prod` build): `hsm-gateway` over mTLS as this service's own pinned leaf (CN `sweep-signer`; the gateway
already lets it read `GET /v1/hot/key`), once at start, with a self-test (`serving`) and a check that the public key hashes to the address. `wallet-service`
(DEV builds only): for a stack whose Hot Wallet is still a software key and so has no gateway. A Hot cutover needs a restart of this service
(the cutover Makefile targets recreate it).

## Database role

`svc_sweepsigner` (`infra/postgres/sweep-signer-role.sql`, `vault.sql`, proofs `sweep-signer-role-check.sql`, `vault-role-check.sql`): `SELECT` on four columns of `wallet_addresses`,
on `wallet_xpubs(chain, xpub)` and on the view `vault.active_secrets` (the active Sealed Secret); nothing else (not `wallet_seeds`), no writes, sessions read-only. Its password is a secret file
(`POSTGRES_PASSWORD_FILE`). The `vault` schema is invisible to `svc_wallet` (wallet-service).

## Container (invariant 16, checked by `make verify`)

`FROM scratch` with one static binary (no shell, no package manager), uid 10002, `read_only`, `cap_drop: [ALL]`, `no-new-privileges`, `mem_limit` with
`memswap_limit` equal, `pids_limit`, `ulimits: core 0, memlock 64 KiB`, no published port, secrets as files. The process calls `PR_SET_DUMPABLE=0` and
sets the core limit to 0 at start. Limit: this protects against a compromise **inside** a container; root on the host can read any process's memory.

## Run and test

```bash
go vet ./... && go test -race -count=1 ./...          # no database or Docker needed
go build -tags prod ./cmd/service                      # drops the dev Hot address source
```

The stack wiring is in `../../e2e/docker-compose.yml` (`make -C e2e up` / `e2e`, `e2e-hsm`) and `../../dev/` (`make up`, `make verify`). The conversion of an existing
mnemonic seed is `docs/runbooks/seed-conversion.md`. The image builds from the repo root (it needs `pkg/metrics`).
