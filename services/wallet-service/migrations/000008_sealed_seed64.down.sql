-- A row created in the new format has no mnemonic blob to fall back to: rolling back would lose that seed. Refuse.
DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM wallet_seeds WHERE sealed_seed IS NULL) THEN
		RAISE EXCEPTION 'wallet_seeds has a seed that exists only in the 64-byte format (sealed_seed64); rolling back migration 000008 would destroy it';
	END IF;
END $$;
ALTER TABLE wallet_seeds DROP CONSTRAINT wallet_seeds_seed64_is_user_derivation_only;
ALTER TABLE wallet_seeds DROP CONSTRAINT wallet_seeds_has_a_seed;
ALTER TABLE wallet_seeds DROP COLUMN sealed_seed64;
ALTER TABLE wallet_seeds ALTER COLUMN sealed_seed SET NOT NULL;
