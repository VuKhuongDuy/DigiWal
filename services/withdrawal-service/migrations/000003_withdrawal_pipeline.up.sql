-- Task 3.3: what a Withdrawal Request accumulates on its way from `reserved` to `confirmed`.
ALTER TABLE withdrawal_requests
    ADD COLUMN nonce               BIGINT,      -- the Hot Wallet nonce wallet-service allocated
    ADD COLUMN signed_raw_tx       TEXT,        -- kept so a crash before broadcast re-sends the SAME tx
    ADD COLUMN chain_id            BIGINT,
    ADD COLUMN broadcast_at        TIMESTAMPTZ,
    ADD COLUMN confirmed_block     BIGINT,      -- block the tx was mined in
    ADD COLUMN settle_ledger_tx_id TEXT,        -- the Settle's ledger transaction (refunds correct it, task 3.5)
    ADD COLUMN confirmed_at        TIMESTAMPTZ;

-- Outbox for withdrawals.broadcast / withdrawals.confirmed: the event row is written in the same
-- transaction as the state change it reports, and published after. A crash in between republishes
-- (at-least-once, Kafka key = withdrawal_id); it can never lose the event.
CREATE TABLE withdrawal_outbox (
    id            BIGSERIAL PRIMARY KEY,
    withdrawal_id UUID NOT NULL REFERENCES withdrawal_requests (withdrawal_id),
    topic         TEXT NOT NULL,
    payload       BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    UNIQUE (withdrawal_id, topic)
);
CREATE INDEX withdrawal_outbox_unpublished ON withdrawal_outbox (id) WHERE published_at IS NULL;
