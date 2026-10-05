package source

import "testing"

func TestCheckSelect(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"select * from entries where amount > 0;",
		"  -- sums\n SELECT account_id, sum(amount) FROM entries GROUP BY 1",
		"/* c */ WITH a AS (SELECT 1) SELECT * FROM a",
		"SELECT created_at, locked_balance FROM accounts",
	}
	for _, s := range ok {
		if err := CheckSelect(s); err != nil {
			t.Errorf("CheckSelect(%q) = %v, want ok", s, err)
		}
	}
	bad := []string{
		"INSERT INTO entries VALUES (1)",
		"UPDATE locks SET status = 'settled'",
		"DELETE FROM entries",
		"TRUNCATE entries",
		"DROP TABLE entries",
		"SELECT 1; DROP TABLE entries",
		"SELECT 1; SELECT 2",
		"WITH d AS (DELETE FROM entries RETURNING *) SELECT * FROM d",
		"WITH u AS (UPDATE locks SET status = 'x' RETURNING 1) SELECT * FROM u",
		"SELECT * FROM locks FOR UPDATE",
		"SELECT set_config('default_transaction_read_only', 'off', false); SET x = 1",
		"SELECT 1 INTO newtable FROM entries; CREATE TABLE x (a int)",
		"COPY entries TO '/tmp/x'",
		"SELECT * FROM wallet_seeds",
		"select sealed_seed from WALLET_SEEDS",
		"SELECT * FROM master_wallet",
		"",
	}
	for _, s := range bad {
		if err := CheckSelect(s); err == nil {
			t.Errorf("CheckSelect(%q) accepted", s)
		}
	}
}
