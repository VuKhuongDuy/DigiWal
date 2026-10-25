-- key-custody-hsm.md invariant 24: Hot nonce state is per (chain_id, hot_address), so a Hot Key change
-- (the HSM key replacing the seed-derived one) never reuses another key's nonce counter or rows.
--
-- Existing rows get hot_address = '' ("legacy, unattributed"): SQL cannot derive the old address. A
-- wallet-service on the software backend attributes them to its own Hot address at start-up
-- (storage.AdoptLegacyHotAddress); a pkcs11 wallet-service never does, so its new address lazily gets
-- a fresh row seeded from that address's on-chain nonce. The DEFAULT '' is kept so older writers (the
-- auditor's test fixtures) still insert; wallet-service always names the address.
ALTER TABLE hot_wallet_nonces ADD COLUMN hot_address TEXT NOT NULL DEFAULT '';
ALTER TABLE hot_wallet_nonces DROP CONSTRAINT hot_wallet_nonces_pkey;
ALTER TABLE hot_wallet_nonces ADD PRIMARY KEY (chain_id, hot_address);

ALTER TABLE hot_wallet_signatures ADD COLUMN hot_address TEXT NOT NULL DEFAULT '';
ALTER TABLE hot_wallet_signatures DROP CONSTRAINT hot_wallet_signatures_chain_id_nonce_key;
ALTER TABLE hot_wallet_signatures ADD CONSTRAINT hot_wallet_signatures_chain_hot_nonce_key UNIQUE (chain_id, hot_address, nonce);
