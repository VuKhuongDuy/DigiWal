# Runbook: bring up a node with the HSM-sealed seed (dev stack, M3c)

Design: `docs/design/key-custody-hsm.md` invariants 17-23, 5.4. Nothing here runs against vm2/vm3 by itself: these are the targets to run there.
Single host: the same targets, everything on one machine. The stack has **no master key** and no software key: the user seed is sealed under a KEK
that never leaves the HSM token.

## Fresh node, in order (every step is idempotent: re-running changes nothing that exists)

| Where | Command | What happens |
|---|---|---|
| operator machine (holds the CA key) | `make -C dev push-certs` | issues every leaf, pushes each node its own (key 0400 owned by the service uid) and the callers' public certs to vm2: `ca-key-off-vm2.md`. Single host: `make -C dev certs` as before |
| vm2 | `make -C dev hsm-setup` | **HSM ceremony**: PIN file, callers pin file (wallet-service, key-admin, sweep-signer leaves), token + **Hot Key + attestation key + KEK (kek-v1)** generated inside SoftHSM (`init-token`), Signing Policy, `hsm/attest-pubkey` pin. A token made before M3c gets its KEK with `hsm-gateway init-kek` (the target does it) and the gateway must be restarted |
| vm2 | copy `dev/hsm/attest-pubkey` to vm3's `dev/hsm/` | the pins live outside the database |
| vm2 | `make -C dev up` | stores, gateway (self-test, vault enabled), ledger, api-gateway |
| vm3 | `make -C dev up` | stores, then **`make init`**: password files, custody migrations (`key-admin migrate`), roles + `vault` schema (`infra/postgres/sweep-signer-role.sql`, `vault.sql`) and their proofs, then **`key-admin init`**, then the services |

`key-admin init`: generate the seed, prove for 50 indexes that private derivation equals public derivation from the xpub, have the gateway seal it (DEK generated and
wrapped in the HSM, AES-GCM in the HSM, random 96-bit nonce owned by the gateway), store it **pending**, have the gateway open it again and compare the xpub, **show the
mnemonic once (write it down: it is the disaster-recovery record)**, have the gateway attest the xpub, store `wallet_xpubs`, re-prove parity against the stored xpub, and only
then flip the row to **active** (the moment `sweep-signer` can read it). It refuses if any sealed secret exists. If it is interrupted after the mnemonic was shown:
`make -C dev key-admin CMD="init -resume"` finishes from the pending row (no new seed). The e2e stack (`make -C e2e up`) runs the identical sequence on throwaway data.

## Rotation (Maintenance Window needed)

KEK administration and re-encryption are refused by the gateway unless a window is open, opened with host access on vm2: `make -C dev maintenance-open` (15 min, max 1 h;
`maintenance-close` ends it). Then on vm3, `make -C dev key-admin CMD=...`:

| CMD | Effect |
|---|---|
| `status` | rows and KEK versions; exit 3 if not initialised |
| `rotate-kek` | generate kek-v(n+1) if needed, re-wrap the DEK **inside the HSM** into a pending row, check it opens to the attested xpub, flip (pending -> active, old -> retired) |
| `rewrap [-kek N]` then `activate` | the same in two steps (nothing flips until `activate`) |
| `rotate-dek` | new DEK, the seed re-encrypted inside the HSM (key-admin never sees it), verify, flip |
| `rollback` | flip back to the newest retired row (its KEK must still exist) |
| `retire-kek N` | kek-vN unwrap-only (refused while the active/pending row uses it); keep it ~30 days |
| `destroy-kek N` | delete it (refused while a live row needs it; `-force` if only a retired row does, which ends rollback) |

Interrupted run: the pending row is inert (sweep-signer reads only the active row); re-run the same command (it reuses a verified pending row or replaces a stale one). Sweeps keep
running during a rotation: both versions open while the old KEK exists.

**Rotating wrappers does not help after a seed leak.** If the seed, the mnemonic, or `sweep-signer`'s memory may have been exposed, the only remedy is a new seed: `seed-rotation.md`.

## Checks and failure modes

`make -C dev verify` (container hardening of the three services, key-file modes and owners, no key or PIN in any environment, no CA key on the node, audit chain, swap and ptrace; SKIP lines say what needs the node itself), `make -C dev vault-role-check` (wallet-service's role `svc_wallet` is denied everything in the `vault` schema).
Gateway down: `sweep-signer` returns 503 and signs nothing (no fallback), deposits are still credited. A tampered row: the gateway answers `authentication_failed`, sweep-signer alerts.
Token files lost: the KEK is gone and so is the seed in the vault; recovery is the paper mnemonic (design invariant 22).
