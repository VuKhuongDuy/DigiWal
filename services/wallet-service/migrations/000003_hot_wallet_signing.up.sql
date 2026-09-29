-- Hot Wallet nonce sequence, one row per chain (withdrawal-flow.md invariants #6, #7).
-- next_nonce is claimed atomically inside the same DB transaction that stores the
-- signature, so a nonce is only ever consumed by a signature we durably recorded.
CREATE TABLE hot_wallet_nonces (
	chain_id          BIGINT PRIMARY KEY,
	next_nonce        BIGINT NOT NULL CHECK (next_nonce >= 0),
	-- Open Nonce Gap: while set, no NEW nonce is allocated, for any caller (invariant #7).
	frozen_from_nonce BIGINT,
	frozen_at         TIMESTAMPTZ
);

-- One signature per idempotency_key (withdrawal-flow.md invariant #16): a retried
-- call replays this row instead of claiming a second nonce.
CREATE TABLE hot_wallet_signatures (
	idempotency_key TEXT PRIMARY KEY,
	chain_id        BIGINT NOT NULL,
	nonce           BIGINT NOT NULL,
	request_hash    TEXT NOT NULL, -- guards against a key being reused for a different request
	signed_raw_tx   BYTEA NOT NULL,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (chain_id, nonce)
);
