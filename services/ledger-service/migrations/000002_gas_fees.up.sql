-- Task 1.9 (ledger.md invariant #12): gas is recorded in the ledger. Every mined tx we sign burns gas
-- out of custody to the validators, which no user Entry accounts for. A Gas Fee is one Ledger
-- Transaction {Gas Expense Account -fee, Chain Account +fee} in the chain's native asset, so the Chain
-- Account keeps matching what custody really holds. The user is never touched.

-- One Gas Expense Account per native asset, not tied to a user. Like the Chain Account it is a
-- counterparty: its total only ever decreases (cumulative gas spent), it is never Locked, and it is
-- exempt from invariant #1's "Available never negative".
ALTER TABLE accounts DROP CONSTRAINT accounts_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check CHECK (kind IN ('user', 'chain', 'gas_expense'));
ALTER TABLE accounts DROP CONSTRAINT accounts_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_check CHECK (
	(kind = 'user' AND user_id IS NOT NULL) OR (kind IN ('chain', 'gas_expense') AND user_id IS NULL));
ALTER TABLE accounts DROP CONSTRAINT accounts_check1;
ALTER TABLE accounts ADD CONSTRAINT accounts_check1 CHECK (
	kind IN ('chain', 'gas_expense') OR (locked_balance >= 0 AND total_balance - locked_balance >= 0));
CREATE UNIQUE INDEX accounts_gas_expense_asset ON accounts (asset_id) WHERE kind = 'gas_expense';

-- Backfill: every registered native asset gets its Gas Expense Account (SyncAssets does it from now on).
INSERT INTO accounts (asset_id, kind) SELECT id, 'gas_expense' FROM assets WHERE asset_type = 'native'
ON CONFLICT (asset_id) WHERE kind = 'gas_expense' DO NOTHING;

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_source_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_source_type_check
	CHECK (source_type IN ('deposit', 'withdrawal_settle', 'correction', 'gas_fee'));
-- Why the gas was spent, for reporting; set only on a gas_fee.
ALTER TABLE ledger_transactions ADD COLUMN purpose TEXT;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_purpose_check CHECK (
	(source_type = 'gas_fee') = (purpose IS NOT NULL)
	AND (purpose IS NULL OR purpose IN ('withdrawal', 'withdrawal_reverted', 'nonce_filler', 'gas_topup', 'sweep', 'treasury_sweep')));
