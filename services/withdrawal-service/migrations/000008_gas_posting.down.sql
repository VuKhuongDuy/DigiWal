DROP INDEX withdrawal_requests_gas_pending;
ALTER TABLE withdrawal_requests
    DROP COLUMN gas_posted_at, DROP COLUMN gas_purpose, DROP COLUMN gas_price, DROP COLUMN gas_used, DROP COLUMN gas_tx_hash;
