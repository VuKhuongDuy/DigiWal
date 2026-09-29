-- One sealed seed per role (wallet-tiering.md invariant #1): the seed that
-- derives user deposit addresses must never be the Hot Wallet's key.
CREATE TABLE wallet_seeds (
	seed_role   TEXT PRIMARY KEY CHECK (seed_role IN ('user_derivation', 'hot_wallet')),
	sealed_seed BYTEA NOT NULL,
	next_index  INT NOT NULL DEFAULT 0, -- per-user derivation counter; unused (0) for hot_wallet
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- An already-initialized seed becomes the user_derivation seed, so existing
-- users' derivation_index values stay valid. No-op if never initialized.
-- The hot_wallet row is deliberately NOT created here: key material is an
-- operator action (init-seed), never something a migration invents.
INSERT INTO wallet_seeds (seed_role, sealed_seed, next_index, created_at)
SELECT 'user_derivation', sealed_seed, next_index, created_at FROM master_wallet WHERE id = 1;

DROP TABLE master_wallet;
