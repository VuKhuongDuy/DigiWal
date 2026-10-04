-- Read-only grants for auditor_ro on this database (A2). Column-level where a table also holds something the
-- auditor must not read. Must match source.Contract exactly (contract_test.go checks both directions).
-- Apply as a superuser or the database owner, after 00_role.sql and after the service has migrated.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM auditor_ro;
GRANT USAGE ON SCHEMA public TO auditor_ro;
-- NOT granted: wallet_seeds, master_wallet (key material), signed_raw_tx columns.
GRANT SELECT (user_id, address) ON wallet_addresses TO auditor_ro;
GRANT SELECT (chain_id, next_nonce, frozen_from_nonce) ON hot_wallet_nonces TO auditor_ro;
GRANT SELECT (idempotency_key, chain_id, nonce, caller_cn, created_at) ON hot_wallet_signatures TO auditor_ro;
GRANT SELECT (idempotency_key, replacement_seq, nonce, tx_hash, max_fee_per_gas, max_priority_fee_per_gas, kind, created_at) ON hot_wallet_replacements TO auditor_ro;
GRANT SELECT (idempotency_key, caller_cn, chain_id) ON hot_wallet_voids TO auditor_ro;
