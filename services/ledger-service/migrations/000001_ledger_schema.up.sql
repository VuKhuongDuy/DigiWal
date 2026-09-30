-- Ledger schema (docs/design/ledger.md).
--
-- Every amount/balance is NUMERIC(78,0), never bigint (ledger.md invariant #10): int64
-- tops out at ~9.22e18, i.e. ~9.2 ETH in wei. 78 digits holds any uint256.

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
	-- total_balance = sum of posted Entries (a cache of the entries table).
	-- locked_balance = sum of active Locks. Available = total - locked, never stored.
	total_balance  NUMERIC(78,0) NOT NULL DEFAULT 0,
	locked_balance NUMERIC(78,0) NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK ((kind = 'user' AND user_id IS NOT NULL) OR (kind = 'chain' AND user_id IS NULL)),
	-- Invariant #1's backstop: a user's Available Balance can never go negative.
	-- The Chain Account is the counterparty of every deposit/withdrawal, so it goes
	-- negative by design, and no Lock is ever placed against it.
	CHECK (kind = 'chain' OR (locked_balance >= 0 AND total_balance - locked_balance >= 0)),
	CHECK (kind = 'user' OR locked_balance = 0)
);
CREATE UNIQUE INDEX accounts_user_asset ON accounts (asset_id, user_id) WHERE kind = 'user';
CREATE UNIQUE INDEX accounts_chain_asset ON accounts (asset_id) WHERE kind = 'chain';

CREATE TABLE ledger_transactions (
	id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	source_type     TEXT NOT NULL CHECK (source_type IN ('deposit', 'withdrawal_settle', 'correction')),
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
-- An entry can be reversed at most once.
CREATE UNIQUE INDEX entries_corrects_once ON entries (corrects_entry_id) WHERE corrects_entry_id IS NOT NULL;
CREATE INDEX entries_account ON entries (account_id);
CREATE INDEX entries_tx ON entries (ledger_transaction_id);

-- A hold against a user's Available Balance (ledger.md "Lock"). Posts no Entry.
-- Keyed 1:1 on the Withdrawal Request's idempotency_key (invariant #6); resolves to
-- settled or released exactly once (invariant #7).
CREATE TABLE locks (
	id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	idempotency_key TEXT NOT NULL UNIQUE,
	account_id      UUID NOT NULL REFERENCES accounts (id),
	amount          NUMERIC(78,0) NOT NULL CHECK (amount > 0),
	status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'settled', 'released')),
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	resolved_at     TIMESTAMPTZ,
	CHECK ((status = 'active') = (resolved_at IS NULL))
);
CREATE INDEX locks_active ON locks (created_at) WHERE status = 'active';

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
