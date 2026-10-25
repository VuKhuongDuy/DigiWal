-- sweep-signer's database role (key-custody-hsm.md M3b, invariants 15 and 28): the least privilege that service needs and nothing else.
-- Run as the database superuser, in the custody database, AFTER wallet-service/key-admin have applied the migrations (the tables must
-- exist), and again whenever a later migration adds a column. Idempotent. The password arrives as a psql variable from a secret FILE:
--   psql -U custody -d custody -v ON_ERROR_STOP=1 -v pw="$(cat sweep-config/db-password)" -f infra/postgres/sweep-signer-role.sql
--
-- What svc_sweepsigner can do:   SELECT user_id, chain, derivation_index, address  FROM wallet_addresses   (which index belongs to whom)
--                                SELECT ... FROM vault.active_secrets   (M3c, granted by vault.sql: the ACTIVE Sealed Secret row, opened only by hsm-gateway)
--                                SELECT chain, xpub                                FROM wallet_xpubs       (the Address Audit's xpub check)
-- What it cannot: write anything, read ANY column of wallet_seeds (the pre-M3c sealed_seed64 is no longer read by anything), the Hot nonce state,
--                 Hot signatures, idempotency rows or any other table, or create objects. Every session is read-only on top of that.
SELECT format('CREATE ROLE svc_sweepsigner LOGIN PASSWORD %L', :'pw') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'svc_sweepsigner') \gexec
SELECT format('ALTER ROLE svc_sweepsigner LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', :'pw') \gexec
ALTER ROLE svc_sweepsigner SET default_transaction_read_only = on;
GRANT CONNECT ON DATABASE custody TO svc_sweepsigner;
REVOKE CREATE ON SCHEMA public FROM svc_sweepsigner;
GRANT USAGE ON SCHEMA public TO svc_sweepsigner;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM svc_sweepsigner;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM svc_sweepsigner;
GRANT SELECT (user_id, chain, derivation_index, address) ON wallet_addresses TO svc_sweepsigner;
GRANT SELECT (chain, xpub) ON wallet_xpubs TO svc_sweepsigner;
