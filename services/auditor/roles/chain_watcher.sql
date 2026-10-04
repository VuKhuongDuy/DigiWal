-- Read-only grants for auditor_ro on this database (A2). Column-level where a table also holds something the
-- auditor must not read. Must match source.Contract exactly (contract_test.go checks both directions).
-- Apply as a superuser or the database owner, after 00_role.sql and after the service has migrated.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM auditor_ro;
GRANT USAGE ON SCHEMA public TO auditor_ro;
GRANT SELECT (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount,
    block_number, block_hash, status, detected_at, confirmed_at) ON observed_transfers TO auditor_ro;
GRANT SELECT ON watermark, scanned_blocks TO auditor_ro;
