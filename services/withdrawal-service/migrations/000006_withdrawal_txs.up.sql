-- Task 3.13 (withdrawal-flow.md invariant #21): Fee Bump. A signed Hot Wallet tx can be re-signed at
-- the same nonce with higher fees; every such version is kept here (replacement_seq >= 1; the
-- original stays in withdrawal_requests.signed_raw_tx). Any version may be mined, never more than
-- one. The pipeline re-broadcasts only the latest; confirmation, the Reaper and `abandon` look at all.
CREATE TABLE withdrawal_txs (
    withdrawal_id   UUID NOT NULL REFERENCES withdrawal_requests (withdrawal_id),
    replacement_seq INT  NOT NULL CHECK (replacement_seq >= 1),
    tx_hash         TEXT NOT NULL,
    raw_tx          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (withdrawal_id, replacement_seq)
);

-- The chain head at which the LATEST version of a request was last sent. NULL means "the latest
-- version has not been sent yet" (a Fee Bump was just stored): the pipeline sends it, then stamps.
-- The Fee Bump waits 20 blocks from this.
ALTER TABLE withdrawal_requests ADD COLUMN sent_head BIGINT;
