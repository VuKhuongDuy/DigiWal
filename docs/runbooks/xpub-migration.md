# LEGACY runbook (superseded by M3c, see dev-hsm-bringup.md): xpub issuance from an old master-key-sealed seed, `key-admin migrate-xpub`

> `migrate-xpub` exists only in a `!prod` build of key-admin and needs the old software master key. A fresh environment uses `key-admin init`.

Design: `docs/design/key-custody-hsm.md` invariants 9-13, M3a. Tool: `services/wallet-service/cmd/key-admin` (image stage `keyadmin`, `!prod`,
never in the wallet-service image). Run once per environment, as the operator, on **vm3** (it needs custody's database and the old software master key).
Nothing existing changes: every address already issued is reproduced bit for bit, and the tool refuses otherwise.
Since M3b the tool reads the seed in either format (the 64-byte one after `key-admin convert-seed`, else the mnemonic; see seed-conversion.md), and an
environment with **no HSM** (dev, e2e) uses `key-admin migrate-xpub -dev-attester`: an ephemeral in-process key attests and its public half is pinned in
`$HSM_DIR/dev-attest-pubkey` (the Makefiles do it). That is not a security claim, it keeps the verification path identical.

**Preconditions.** `hsm-gateway` serving on a token that has an attestation key (a token from before M3a: `hsm-gateway init-attest-key`; `make hsm-setup
SIGNER_BACKEND=pkcs11` does it). The key-admin leaf is issued and pinned in the gateway's `callers.yaml` (`make push-certs` on vm2, re-run
`infra/hsm/prepare.sh`, restart the gateway). The `user_derivation` seed exists.

| # | Step | Command | Notes |
|---|------|---------|-------|
| 1 | Pin the attestation public key **outside the database** | gateway node: `make hsm-setup SIGNER_BACKEND=pkcs11` writes `$HSM_DIR/attest-pubkey` (hex of the 65-byte P-256 point from `init-token`); copy that file to vm3's `$HSM_DIR` | Public, but it is the root of trust: compare it with the ceremony printout out of band. A changed key is never accepted silently |
| 2 | Dry run | `make xpub-migrate SIGNER_BACKEND=pkcs11 DRY_RUN=1` | Derives the xpub, proves **every** `wallet_addresses` row is reproduced and `wallet_seeds.next_index` is past the highest index, checks the gateway's key equals the pin. Asks the gateway to attest nothing, writes nothing. Prints the xpub fingerprint, never the xpub |
| 3 | Migrate | `make xpub-migrate SIGNER_BACKEND=pkcs11` | Gateway attests `(evm, m/44'/60'/0', xpub)`; the attestation is verified against the pin; `wallet_xpubs` is stored with `next_index` carried over. Re-running verifies and changes nothing; a different xpub is refused |
| 4 | Switch issuance | `make up SIGNER_BACKEND=pkcs11` | wallet-service starts with `XPUB_ATTEST_PUBKEY_FILE`; its log says `deposit addresses issue from the attested xpub`. Issuance verifies the attestation on every new address |

**Refusals you may see.** `REFUSED ... does not reproduce the existing addresses`: the sealed seed is not the one that issued the rows (wrong software master key,
restored wrong backup): stop, nothing was attested. `not the one pinned`: wrong token or a stale pin file. `counter is behind`: `wallet_seeds.next_index` is lower than a used
index: fix the data first, issuing would reuse an address.

**Failure at runtime.** If the stored xpub or attestation no longer verifies (database swap, wrong pin) wallet-service logs `ALERT xpub attestation failed`, latches new-address
creation off until restart (`503 issuance_unavailable`) and keeps serving stored addresses. Treat it as an incident: compare `wallet_xpubs` with the backup, check the pin file, then restart.
An attacker who controls both the database and the pin file is not stopped here; that case is detected by the seed holder (invariant 11, M3b).

**Rollback.** `migrate` down of migration 000007 copies the xpub counter back into `wallet_seeds.next_index` before dropping the table; unset `XPUB_ATTEST_PUBKEY_FILE` and
wallet-service issues from the seed again (same addresses, same sequence). **Not after M3b**: wallet-service no longer has a seed, so a rollback of the xpub means rolling back to the M3a image. Not covered here: Solana / ed25519 (no public derivation exists), seed rotation (M3c).
