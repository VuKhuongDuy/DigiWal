# LEGACY runbook (superseded by M3c, see dev-hsm-bringup.md): user_derivation seed, mnemonic -> 64-byte Seed, `key-admin convert-seed`

> Since M3c nothing reads the master-key-sealed `sealed_seed64` any more: sweep-signer opens the HSM-sealed vault row. This runbook documents a `!prod` tool kept only for rehearsing the old migrations; a fresh environment uses `key-admin init`.

Design: `docs/design/key-custody-hsm.md` K11, invariants 14-15, M3b. Tool: `services/wallet-service/cmd/key-admin` (image stage `keyadmin`, `!prod`).
Run once per environment that was created before M3b (its seed is a sealed **mnemonic**), as the operator, on **vm3** (custody's database
and the old software master key). A new environment skips this: `key-admin init` creates the sealed seed directly.

**Why.** `sweep-signer` is the only place the seed exists in plaintext, and it must be able to zero it: the 64 raw BIP39 seed bytes can be
wiped, a mnemonic (an immutable Go string, and a library `Wallet` that keeps derived keys) cannot. So the seed is stored as 64 bytes and
derived with btcd `hdkeychain` + `btcec` on `[]byte`.

**Format of the new row** (`wallet_seeds.sealed_seed64`, migration 000008): AES-256-GCM under the **same** old software master key, `nonce || ciphertext`,
AAD = `user_derivation|sweep|1` (role | purpose | format version; the M3c AAD extends this prefix). A mnemonic-format blob (no AAD) or a blob of
another role does not authenticate as a Seed. The old mnemonic blob (`sealed_seed`) is **not touched**: it is the rollback until M3c has served
real Sweeps.

| # | Step | Command | Notes |
|---|------|---------|-------|
| 1 | Dry run | `make convert-seed DRY_RUN=1` (`dev/` or `e2e/`) | Derives the seed, then proves (a) the new derivation reproduces **every** `wallet_addresses` row, (b) it equals the old library (go-ethereum-hdwallet) for indexes 0-99, (c) if `wallet_xpubs` exists its account xpub is the Seed's, (d) the sealed result opens back to the same bytes and not without the AAD. Writes nothing. Prints counts and an xpub fingerprint, never the seed or the mnemonic |
| 2 | Convert | `make convert-seed` | Same proof, then writes `sealed_seed64` (only where it is still NULL). Re-running only re-verifies |
| 3 | Role and files | `make sweep-setup` | Writes `master-key` and `db-password` as secret FILES and the pinned `sweeper` leaf into `$SWEEP_DIR`, creates the read-only database role `svc_sweepsigner` (`infra/postgres/sweep-signer-role.sql`) and runs the proof of its grants (`sweep-signer-role-check.sql`) |
| 4 | Start | `make up` | `sweep-signer` starts, pins the Hot address (from `hsm-gateway`; dev profile: wallet-service), runs its first Address Audit pass; sweeps are refused until that pass is clean (`sweep_signer_ready`) |

**Refusals you may see.** `REFUSED ... does not reproduce` / `the new derivation gives X, the old one Y`: the seed is not the one that issued the rows
(wrong software master key, wrong backup) or the derivation differs: stop, nothing was written, do not start `sweep-signer`. `not the xpub stored in wallet_xpubs`:
the attested xpub belongs to another seed: incident, see xpub-migration.md. `nothing to convert`: the environment is new (use `init-seed`).

**Rollback.** `sealed_seed64` is additive. To go back to the pre-M3b arrangement drop the column with migration 000008's down (refused if any seed exists
only in the new format) and run the previous images; the mnemonic blob and the key are unchanged.

**What stays true after this.** (Superseded by M3c.) Nothing in the running system holds the old master key: the user seed is sealed under a KEK in the HSM.
