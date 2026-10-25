-- key-custody-hsm.md M3b, K11: the user_derivation Seed is stored as the 64 raw bytes of the BIP39 seed (not the mnemonic), so
-- sweep-signer can hold it in one zeroable page and never make a string of it.
--
--   sealed_seed64  AES-256-GCM, nonce || ciphertext, AAD "user_derivation|sweep|1" (role | purpose | format version; M3c's AAD
--                  extends this prefix). It is NULL until `key-admin convert-seed` (existing environments) or `key-admin
--                  init-seed` (new ones) writes it. Only sweep-signer's role (svc_sweepsigner) can read it, by a column grant.
--   sealed_seed    the mnemonic-format blob. KEPT, untouched by the conversion, as the rollback until M3c has served real Sweeps
--                  (design 5.5); it becomes nullable so a new environment need not have one.
ALTER TABLE wallet_seeds ALTER COLUMN sealed_seed DROP NOT NULL;
ALTER TABLE wallet_seeds ADD COLUMN sealed_seed64 BYTEA;
ALTER TABLE wallet_seeds ADD CONSTRAINT wallet_seeds_has_a_seed CHECK (sealed_seed IS NOT NULL OR sealed_seed64 IS NOT NULL);
ALTER TABLE wallet_seeds ADD CONSTRAINT wallet_seeds_seed64_is_user_derivation_only CHECK (sealed_seed64 IS NULL OR seed_role = 'user_derivation');
