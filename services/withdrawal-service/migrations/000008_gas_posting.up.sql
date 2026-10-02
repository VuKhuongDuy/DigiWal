-- Task 3.15 (withdrawal-flow.md invariant #26, ledger.md invariant #12): the gas a mined tx of ours
-- spent is posted to the ledger once. The numbers are written in the SAME transaction as the state
-- change that makes the tx final (confirmed, failed/tx_reverted, or Released after a Nonce Filler
-- mined), so a crash can never lose them; a separate stage then posts them until ledger-service
-- answers 200 and stamps gas_posted_at. Pending = gas_purpose set and gas_posted_at not.
-- Requests that became final before this migration have no gas_purpose: nothing is posted for them.
ALTER TABLE withdrawal_requests
    ADD COLUMN gas_tx_hash   TEXT,
    ADD COLUMN gas_used      BIGINT,
    ADD COLUMN gas_price     NUMERIC(78,0),   -- effectiveGasPrice, wei
    ADD COLUMN gas_purpose   TEXT CHECK (gas_purpose IN ('withdrawal', 'withdrawal_reverted', 'nonce_filler')),
    ADD COLUMN gas_posted_at TIMESTAMPTZ;
CREATE INDEX withdrawal_requests_gas_pending ON withdrawal_requests (updated_at)
    WHERE gas_purpose IS NOT NULL AND gas_posted_at IS NULL;
