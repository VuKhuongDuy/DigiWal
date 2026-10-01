DROP TABLE IF EXISTS nonce_gaps;
ALTER TABLE withdrawal_requests DROP COLUMN IF EXISTS disputed_from, DROP COLUMN IF EXISTS disputed_at;
