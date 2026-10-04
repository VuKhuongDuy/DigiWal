-- Read-only grants for auditor_ro on this database (A2). Column-level where a table also holds something the
-- auditor must not read. Must match source.Contract exactly (contract_test.go checks both directions).
-- Apply as a superuser or the database owner, after 00_role.sql and after the service has migrated.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM auditor_ro;
GRANT USAGE ON SCHEMA public TO auditor_ro;
-- NOT granted: signed_raw_tx, idempotency_key, raw_tx, outbox payloads.
GRANT SELECT (withdrawal_id, user_id, asset_id, amount, to_address, status, reject_reason, tx_hash,
    created_at, updated_at, nonce, chain_id, broadcast_at, confirmed_block, settle_ledger_tx_id, confirmed_at,
    disputed_from, disputed_at, broadcast_head_seen, sent_head, gas_tx_hash, gas_used, gas_price, gas_purpose, gas_posted_at)
    ON withdrawal_requests TO auditor_ro;
GRANT SELECT (withdrawal_id, replacement_seq, tx_hash, created_at) ON withdrawal_txs TO auditor_ro;
GRANT SELECT (withdrawal_id, topic, created_at, published_at) ON withdrawal_outbox TO auditor_ro;
GRANT SELECT (chain_id, from_nonce, tx_hash, detected_at, reported_at) ON nonce_gaps TO auditor_ro;
