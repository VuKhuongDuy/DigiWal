-- Only valid while no gas_fee transaction exists (entries are append-only and cannot be deleted).
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_purpose_check;
ALTER TABLE ledger_transactions DROP COLUMN purpose;
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_source_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_source_type_check
	CHECK (source_type IN ('deposit', 'withdrawal_settle', 'correction'));
DROP INDEX accounts_gas_expense_asset;
DELETE FROM accounts WHERE kind = 'gas_expense';
ALTER TABLE accounts DROP CONSTRAINT accounts_check1;
ALTER TABLE accounts ADD CONSTRAINT accounts_check1 CHECK (kind = 'chain' OR (locked_balance >= 0 AND total_balance - locked_balance >= 0));
ALTER TABLE accounts DROP CONSTRAINT accounts_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_check CHECK (
	(kind = 'user' AND user_id IS NOT NULL) OR (kind = 'chain' AND user_id IS NULL));
ALTER TABLE accounts DROP CONSTRAINT accounts_kind_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check CHECK (kind IN ('user', 'chain'));
