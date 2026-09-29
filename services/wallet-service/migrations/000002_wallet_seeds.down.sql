CREATE TABLE master_wallet (
	id             INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
	sealed_seed    BYTEA NOT NULL,
	next_index     INT NOT NULL DEFAULT 0,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO master_wallet (id, sealed_seed, next_index, created_at)
SELECT 1, sealed_seed, next_index, created_at FROM wallet_seeds WHERE seed_role = 'user_derivation';

-- The hot_wallet row has nowhere to go in the old schema and is dropped.
DROP TABLE wallet_seeds;
