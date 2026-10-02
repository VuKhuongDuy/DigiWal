-- chain-watcher's own state (docs/design/deposit-flow.md, Open Question #1: approved).
-- Not disposable like a cache: losing the watermark or the pending transfers costs missed
-- deposits or a full re-scan.

-- How far the chain has been scanned, one row per chain. It only advances past blocks whose
-- every matching transfer has been durably recorded (invariant #4).
CREATE TABLE watermark (
	chain_id     BIGINT PRIMARY KEY,
	block_number BIGINT NOT NULL CHECK (block_number >= 0),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hashes of the most recently scanned blocks. A reorg replaces a suffix of the chain, so
-- comparing the tip's stored hash with the node's tells us the range we scanned is stale.
CREATE TABLE scanned_blocks (
	chain_id     BIGINT NOT NULL,
	block_number BIGINT NOT NULL,
	block_hash   TEXT NOT NULL,
	PRIMARY KEY (chain_id, block_number)
);

-- Observed Transfer, keyed by (chain_id, tx_hash, log_index); log_index = -1 for native coin.
-- Amounts are NUMERIC(78,0), never bigint (an int64 holds only ~9.2 ETH of wei).
CREATE TABLE observed_transfers (
	chain_id               BIGINT NOT NULL,
	tx_hash                TEXT NOT NULL,
	log_index              INT NOT NULL CHECK (log_index >= -1),
	asset_id               UUID NOT NULL,
	from_address           TEXT NOT NULL,
	to_address             TEXT NOT NULL,
	user_id                TEXT NOT NULL CHECK (user_id <> ''),
	amount                 NUMERIC(78,0) NOT NULL CHECK (amount > 0),
	block_number           BIGINT NOT NULL,
	block_hash             TEXT NOT NULL,
	status                 TEXT NOT NULL CHECK (status IN ('pending', 'confirmed')),
	detected_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	confirmed_at           TIMESTAMPTZ,
	-- Publish bookkeeping: a row is only marked published after Kafka acknowledged it, so a
	-- crash between "recorded" and "published" is repaired on the next pass (at-least-once).
	detected_published_at  TIMESTAMPTZ,
	confirmed_published_at TIMESTAMPTZ,
	PRIMARY KEY (chain_id, tx_hash, log_index),
	CHECK ((status = 'confirmed') = (confirmed_at IS NOT NULL)),
	CHECK (confirmed_published_at IS NULL OR status = 'confirmed')
);
CREATE INDEX observed_transfers_pending ON observed_transfers (chain_id, block_number) WHERE status = 'pending';
CREATE INDEX observed_transfers_unpublished ON observed_transfers (chain_id)
	WHERE detected_published_at IS NULL OR (status = 'confirmed' AND confirmed_published_at IS NULL);
