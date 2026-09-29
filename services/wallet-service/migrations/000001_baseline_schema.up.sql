-- Baseline: reproduces the schema that used to be applied inline by
-- storage.Connect. IF NOT EXISTS so it is a no-op against an already
-- provisioned dev DB (it just gets golang-migrate's tracking table filled).
CREATE TABLE IF NOT EXISTS master_wallet (
	id             INT PRIMARY KEY DEFAULT 1 CHECK (id = 1), -- singleton row
	sealed_seed    BYTEA NOT NULL,
	next_index     INT NOT NULL DEFAULT 0,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wallet_addresses (
	user_id          TEXT NOT NULL,
	chain            TEXT NOT NULL DEFAULT 'evm',
	derivation_index INT NOT NULL,
	address          TEXT NOT NULL UNIQUE,
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (user_id, chain)
);
