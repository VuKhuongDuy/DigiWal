-- Read-only grants for auditor_ro on this database (A2). Column-level where a table also holds something the
-- auditor must not read. Must match source.Contract exactly (contract_test.go checks both directions).
-- Apply as a superuser or the database owner, after 00_role.sql and after the service has migrated.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM auditor_ro;
GRANT USAGE ON SCHEMA public TO auditor_ro;
GRANT SELECT (id, asset_type, symbol, decimals) ON assets TO auditor_ro;
GRANT SELECT (id, asset_id, kind, user_id, total_balance, locked_balance) ON accounts TO auditor_ro;
GRANT SELECT (id, source_type, idempotency_key, chain_id, tx_hash, log_index, created_at, purpose) ON ledger_transactions TO auditor_ro;
GRANT SELECT (id, ledger_transaction_id, account_id, amount, corrects_entry_id, created_at) ON entries TO auditor_ro;
GRANT SELECT (id, idempotency_key, account_id, amount, status, created_at, resolved_at) ON locks TO auditor_ro;
GRANT SELECT (chain_id, tx_hash, log_index, asset_id, amount, status) ON flagged_deposits TO auditor_ro;
