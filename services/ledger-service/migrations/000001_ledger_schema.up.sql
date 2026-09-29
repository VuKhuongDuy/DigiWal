-- Ledger schema (docs/design/ledger.md).
--
-- Amounts are NUMERIC(78,0), not the bigint ledger.md sketches: int64 tops out at
-- ~9.22e18, i.e. ~9.2 ETH in wei, so a bigint balance would overflow on a
-- modest deposit. 78 digits holds any uint256.

-- Asset Registry mirror. Rows come from the canonical infra/assets.yaml at
-- startup; "active = false" means the asset left the registry and is no longer accepted.
CREATE TABLE assets (
	id                    UUID PRIMARY KEY,
	asset_type            TEXT NOT NULL CHECK (asset_type IN ('native', 'erc20')),
	contract_address      TEXT,
	symbol                TEXT NOT NULL,
	decimals              INT NOT NULL CHECK (decimals BETWEEN 0 AND 77),
	hot_floor             NUMERIC(78,0) NOT NULL CHECK (hot_floor >= 0),
	hot_ceiling           NUMERIC(78,0) NOT NULL CHECK (hot_ceiling >= hot_floor),
	min_withdrawal_amount NUMERIC(78,0) NOT NULL CHECK (min_withdrawal_amount >= 0),
	active                BOOLEAN NOT NULL DEFAULT TRUE,
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK ((asset_type = 'erc20') = (contract_address IS NOT NULL))
);

-- One account per (user, asset), plus exactly one Chain Account per asset.
CREATE TABLE accounts (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	asset_id   UUID NOT NULL REFERENCES assets (id), -- an unregistered asset can never have an account (invariant #5)
	kind       TEXT NOT NULL CHECK (kind IN ('user', 'chain')),
	user_id    TEXT,
	balance    NUMERIC(78,0) NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK ((kind = 'user' AND user_id IS NOT NULL) OR (kind = 'chain' AND user_id IS NULL)),
	-- The Chain Account is the counterparty of every deposit/withdrawal so it is
	-- negative by construction; only user accounts are bound by invariant #1.
	CHECK (kind = 'chain' OR balance >= 0)
);
CREATE UNIQUE INDEX accounts_user_asset ON accounts (asset_id, user_id) WHERE kind = 'user';
CREATE UNIQUE INDEX accounts_chain_asset ON accounts (asset_id) WHERE kind = 'chain';

CREATE TABLE ledger_transactions (
	id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	source_type     TEXT NOT NULL CHECK (source_type IN ('deposit', 'withdrawal', 'correction')),
	idempotency_key TEXT NOT NULL UNIQUE, -- invariant #3
	request_hash    TEXT NOT NULL,        -- a replay must carry the same content
	-- Deposits also carry their natural key (ledger.md invariant #3); -1 = native coin.
	chain_id        BIGINT,
	tx_hash         TEXT,
	log_index       BIGINT,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (chain_id, tx_hash, log_index)
);

CREATE TABLE entries (
	id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	ledger_transaction_id UUID NOT NULL REFERENCES ledger_transactions (id),
	account_id            UUID NOT NULL REFERENCES accounts (id),
	amount                NUMERIC(78,0) NOT NULL CHECK (amount <> 0), -- signed, minor units
	corrects_entry_id     UUID REFERENCES entries (id),
	created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX entries_account ON entries (account_id);
CREATE INDEX entries_tx ON entries (ledger_transaction_id);

-- Invariant #4: entries are immutable. A correction is a new opposite entry.
CREATE FUNCTION entries_are_immutable() RETURNS trigger AS $$
BEGIN
	RAISE EXCEPTION 'entries are append-only (ledger.md invariant #4)';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER entries_no_update_delete BEFORE UPDATE OR DELETE ON entries
	FOR EACH ROW EXECUTE FUNCTION entries_are_immutable();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON entries
	FOR EACH STATEMENT EXECUTE FUNCTION entries_are_immutable();

-- compliance-screening.md: a deposit from a Screening List address is recorded here
-- and never becomes a Ledger Transaction automatically.
CREATE TABLE flagged_deposits (
	chain_id     BIGINT NOT NULL,
	tx_hash      TEXT NOT NULL,
	log_index    BIGINT NOT NULL,
	asset_id     UUID NOT NULL,
	from_address TEXT NOT NULL,
	to_address   TEXT NOT NULL,
	amount       NUMERIC(78,0) NOT NULL,
	block_number BIGINT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	resolved_at  TIMESTAMPTZ,
	PRIMARY KEY (chain_id, tx_hash, log_index) -- invariant #4: redelivery can't duplicate a flag
);

-- Events that can never be applied (malformed, unknown asset/address). Kept instead
-- of dropped so nothing is lost silently; no design doc covers dead letters yet.
CREATE TABLE failed_events (
	id          BIGSERIAL PRIMARY KEY,
	topic       TEXT NOT NULL,
	message_key TEXT,
	payload     BYTEA NOT NULL,
	error       TEXT NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
