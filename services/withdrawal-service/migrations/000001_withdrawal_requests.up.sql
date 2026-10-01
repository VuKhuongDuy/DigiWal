-- Withdrawal Requests (docs/design/withdrawal-flow.md). One row per request, created before
-- anything else runs (invariant #11) and keyed per user (invariant #20): the client's
-- idempotency_key is unique only within its user_id, and withdrawal_id (server-minted) is
-- what every downstream key is derived from (Withdrawal Key = 'withdrawal:' || withdrawal_id).
CREATE TABLE withdrawal_requests (
    withdrawal_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    asset_id        TEXT NOT NULL,
    amount          NUMERIC(78,0) NOT NULL CHECK (amount > 0),
    to_address      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'requested' CHECK (status IN (
                        'requested', 'validated', 'reserved', 'signed', 'broadcast', 'confirmed',
                        'rejected', 'failed', 'disputed')),
    reject_reason   TEXT,
    tx_hash         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);

-- The Reaper (invariant #17) scans by status and age.
CREATE INDEX withdrawal_requests_status_updated ON withdrawal_requests (status, updated_at);
