-- Proof that svc_sweepsigner has exactly the grants of sweep-signer-role.sql (run as the superuser, same database):
--   psql -U custody -d custody -v ON_ERROR_STOP=1 -f infra/postgres/sweep-signer-role-check.sql
-- Every "must work" statement runs, every "must be denied" statement must raise insufficient_privilege; anything else fails the run.
SET ROLE svc_sweepsigner;
DO $$
DECLARE
	denied text[] := ARRAY[
		'SELECT sealed_seed FROM wallet_seeds',
		'SELECT next_index FROM wallet_seeds',
		'SELECT * FROM wallet_seeds',
		'SELECT seed_role, sealed_seed64 FROM wallet_seeds',
		'SELECT * FROM vault.sealed_secrets',
		'SELECT status FROM vault.sealed_secrets',
		'INSERT INTO vault.sealed_secrets (role, version, status, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad) VALUES (''x'', 1, ''active'', ''p'', ''kek-v1'', 1, ''\x00'', ''\x000000000000000000000000'', ''\x00'', ''\x00'')',
		'UPDATE vault.sealed_secrets SET status = ''retired''',
		'DELETE FROM vault.sealed_secrets',
		'SELECT attestation FROM wallet_xpubs',
		'SELECT next_index FROM wallet_xpubs',
		'SELECT * FROM wallet_addresses',
		'SELECT * FROM hot_wallet_nonces',
		'SELECT * FROM hot_wallet_signatures',
		'INSERT INTO wallet_addresses (user_id, chain, derivation_index, address) VALUES (''x'', ''evm'', 1, ''0x'')',
		'UPDATE wallet_seeds SET sealed_seed64 = NULL',
		'DELETE FROM wallet_addresses',
		'UPDATE wallet_xpubs SET xpub = xpub',
		'CREATE TABLE sweep_signer_must_not_create (a int)'
	];
	q text;
	n bigint;
BEGIN
	-- must work
	SELECT count(*) INTO n FROM (SELECT user_id, chain, derivation_index, address FROM wallet_addresses) a;
	SELECT count(*) INTO n FROM (SELECT role, version, wrapped_dek, gcm_nonce, ciphertext, aad FROM vault.active_secrets) a;
	SELECT count(*) INTO n FROM (SELECT chain, xpub FROM wallet_xpubs) a;
	-- must be denied
	FOREACH q IN ARRAY denied LOOP
		BEGIN
			EXECUTE q;
			RAISE EXCEPTION 'svc_sweepsigner was ALLOWED: %', q;
		EXCEPTION WHEN insufficient_privilege THEN
			NULL;
		END;
	END LOOP;
	RAISE NOTICE 'svc_sweepsigner: 3 reads allowed (addresses, xpub, vault.active_secrets), % statements denied', array_length(denied, 1);
END $$;
RESET ROLE;
