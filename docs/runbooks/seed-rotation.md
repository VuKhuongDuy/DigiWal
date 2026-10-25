# Runbook: Seed rotation (a new user_derivation seed, new deposit addresses)

Design: `docs/design/key-custody-hsm.md` invariant 20. **Say it plainly: `rotate-kek`, `rewrap` and `rotate-dek` change only the wrappers around the same seed. They do nothing against an
attacker who already holds the seed or the mnemonic.** A leaked seed means every deposit address derived from it is compromised, now and for every future deposit to it. The only response is a
new seed, triggered by the Tech Lead after an incident.

## What the code supports today, and what it does not (read before you rely on this)

The vault, `key-admin init` and `sweep-signer` support ONE seed: one xpub row (`wallet_xpubs`), one active `user_derivation` row, one address counter. The design's "keep the old seed
retired but unsealable for old addresses, new users on the new seed" (a `seed_version` per address and per xpub row) is **not built in M3c**. So the supported seed rotation is drain, then replace:

1. Stop issuing: stop `wallet-service` (no new deposit addresses) and the simulator/API traffic that creates users.
2. Sweep every funded deposit address to Hot with the OLD seed: let `sweeper` finish (watch `sweep_signer_*` metrics and the on-chain balances of every `wallet_addresses` row); deposits can still arrive at old
   addresses, so keep the old seed available until a grace period without incoming funds has passed. Open a Maintenance Window only when you touch the vault.
3. Record the final state: back up `wallet_addresses` (it maps users to old addresses and must be kept for audit and for late deposits).
4. Replace the seed: with the old row's funds swept, `key-admin` cannot create a second seed over the first (`init` refuses by design). The replacement is an operator procedure on the database:
   in a Maintenance Window, as the Postgres admin, retire the old row (`UPDATE vault.sealed_secrets SET status='retired' ...`), clear `wallet_xpubs` for the chain, truncate no address history, then run `key-admin init`
   for the new seed (it needs `wallet_addresses` to be reproducible by the new seed: **it will refuse while old address rows exist**, which is the safety check). This step therefore needs a code change (a `seed_version`
   column on `wallet_addresses` and `wallet_xpubs`) before it can be done with live users. **That is an open item for the Tech Lead, not something this runbook can paper over.**
5. Restart `sweep-signer` and `wallet-service`, check `make verify` and the Address Audit (`sweep_signer_ready`).

For a throwaway dev stack the simple and complete rotation is `make -C dev reset` (or `make -C e2e down` then `up`): new token, new KEK, new seed, new addresses.
