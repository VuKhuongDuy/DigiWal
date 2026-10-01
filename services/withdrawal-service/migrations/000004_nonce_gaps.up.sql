-- Task 3.4: Nonce Gap detection (withdrawal-flow.md invariants #7-#9, #19).
-- A request moved to `disputed` keeps where it came from: whether it had Settled (`confirmed`)
-- decides how an operator resolves it (a refund, task 3.5, vs a plain Release).
ALTER TABLE withdrawal_requests
    ADD COLUMN disputed_from TEXT CHECK (disputed_from IN ('signed', 'broadcast', 'confirmed')),
    ADD COLUMN disputed_at   TIMESTAMPTZ;

-- Each detected gap, and whether wallet-service has acknowledged the freeze yet: detection is
-- recorded first (with the disputes, in one transaction), the report is retried until it lands.
CREATE TABLE nonce_gaps (
    chain_id    BIGINT NOT NULL,
    from_nonce  BIGINT NOT NULL CHECK (from_nonce >= 0),
    tx_hash     TEXT NOT NULL,          -- the confirmed tx that disappeared
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reported_at TIMESTAMPTZ,            -- wallet-service answered 200
    PRIMARY KEY (chain_id, from_nonce)
);
