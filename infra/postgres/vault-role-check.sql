-- Proof that wallet-service's database role is locked out of the vault (key-custody-hsm.md invariant 28). Run as the superuser, same database:
--   psql -U custody -d custody -v ON_ERROR_STOP=1 -v walletrole=svc_wallet -f infra/postgres/vault-role-check.sql
-- walletrole is the login wallet-service connects as (dev: svc_wallet, which owns the custody database). Every statement must raise
-- insufficient_privilege; anything else, including success, fails the run. It also proves the same for the two roles that must NOT write the vault
-- (svc_sweepsigner can only read the active view) and that the vault schema grants nothing to PUBLIC.
SET ROLE :"walletrole";
DO $$
DECLARE
	denied text[] := ARRAY[
		'SELECT * FROM vault.sealed_secrets',
		'SELECT * FROM vault.active_secrets',
		'SELECT ciphertext FROM vault.sealed_secrets',
		'INSERT INTO vault.sealed_secrets (role, version, status, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad) VALUES (''user_derivation'', 99, ''pending'', ''sweep'', ''kek-v1'', 1, ''\x00'', ''\x000000000000000000000000'', ''\x00'', ''\x00'')',
		'UPDATE vault.sealed_secrets SET status = ''retired''',
		'DELETE FROM vault.sealed_secrets',
		'TRUNCATE vault.sealed_secrets',
		'CREATE TABLE vault.wallet_must_not_create (a int)',
		'DROP SCHEMA vault CASCADE',
		'GRANT USAGE ON SCHEMA vault TO PUBLIC'
	];
	q text;
BEGIN
	FOREACH q IN ARRAY denied LOOP
		BEGIN
			EXECUTE q;
			RAISE EXCEPTION 'the wallet-service role was ALLOWED: %', q;
		EXCEPTION WHEN insufficient_privilege THEN
			NULL;
		END;
	END LOOP;
	RAISE NOTICE 'wallet-service role: % vault statements denied', array_length(denied, 1);
END $$;
RESET ROLE;
SELECT 'vault schema ACL: ' || coalesce(nspacl::text, '(none: owner only)') FROM pg_namespace WHERE nspname = 'vault';
