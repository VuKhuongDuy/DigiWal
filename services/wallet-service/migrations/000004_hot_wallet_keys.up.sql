-- Task 3.13 (withdrawal-flow.md invariants #21, #22, #23): never strand a Hot Wallet nonce.

-- Who signed: the verified client-certificate CN at sign time. Lookup, void and replace act only on
-- rows whose caller_cn equals the caller's. Rows signed before this migration stay NULL, which is
-- "not the caller's" for every one of them (a replay of the same key by its sender still works).
ALTER TABLE hot_wallet_signatures ADD COLUMN caller_cn TEXT;

-- Signature Void tombstones: a key promised never to be signed. Written under the same
-- hot_wallet_nonces row lock signing takes, so a void and a sign for one key can never both win.
CREATE TABLE hot_wallet_voids (
	idempotency_key TEXT PRIMARY KEY,
	caller_cn       TEXT NOT NULL,
	chain_id        BIGINT NOT NULL,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every re-signed version of a signed key: same nonce, same destination/amount/data, higher fees
-- (kind 'fee_bump'), or an operator's 0-value Hot -> Hot Nonce Filler (kind 'cancel'). The original
-- stays in hot_wallet_signatures, whose UNIQUE (chain_id, nonce) is why these need their own table.
-- No new nonce is ever allocated for a replacement.
CREATE TABLE hot_wallet_replacements (
	idempotency_key          TEXT NOT NULL REFERENCES hot_wallet_signatures (idempotency_key),
	replacement_seq          INT NOT NULL CHECK (replacement_seq >= 1),
	nonce                    BIGINT NOT NULL,
	signed_raw_tx            BYTEA NOT NULL,
	tx_hash                  TEXT NOT NULL,
	max_fee_per_gas          NUMERIC(78,0) NOT NULL,
	max_priority_fee_per_gas NUMERIC(78,0) NOT NULL,
	kind                     TEXT NOT NULL CHECK (kind IN ('fee_bump', 'cancel')),
	created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (idempotency_key, replacement_seq)
);
