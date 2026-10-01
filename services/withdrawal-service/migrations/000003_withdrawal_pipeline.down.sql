DROP TABLE IF EXISTS withdrawal_outbox;
ALTER TABLE withdrawal_requests
    DROP COLUMN IF EXISTS nonce, DROP COLUMN IF EXISTS signed_raw_tx, DROP COLUMN IF EXISTS chain_id,
    DROP COLUMN IF EXISTS broadcast_at, DROP COLUMN IF EXISTS confirmed_block,
    DROP COLUMN IF EXISTS settle_ledger_tx_id, DROP COLUMN IF EXISTS confirmed_at;
