package ledger

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
)

const gasTx = "0x1111111111111111111111111111111111111111111111111111111111111111"

func gasFee(hash string, used uint64, price string, purpose string) GasFee {
	return GasFee{ChainID: 31337, TxHash: hash, GasUsed: used, EffectiveGasPrice: n(price), Purpose: purpose}
}

// account totals for the native asset's chain and gas_expense accounts.
func nativeTotals(t *testing.T, s *Store) (chainTotal, gasTotal string) {
	t.Helper()
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT total_balance::text FROM accounts WHERE kind = 'chain' AND asset_id = $1`, eth).Scan(&chainTotal); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT total_balance::text FROM accounts WHERE kind = 'gas_expense' AND asset_id = $1`, eth).Scan(&gasTotal); err != nil {
		t.Fatal(err)
	}
	return
}

// invariant #12: fee = gas_used × effective_gas_price, {gas_expense -fee, chain +fee}, sum zero, no user touched.
func TestGasFee_PostsTheFeeBetweenTheGasExpenseAndChainAccounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000")) // alice 1000, chain -1000
	res, err := s.PostGasFee(ctx, gasFee(gasTx, 21000, "2000000000", "withdrawal"))
	if err != nil {
		t.Fatal(err)
	}
	fee := "42000000000000"
	if res.Amount.String() != fee || res.TransactionID == "" {
		t.Fatalf("%+v", res)
	}
	chainTotal, gasTotal := nativeTotals(t, s)
	if chainTotal != new(big.Int).Sub(n("-1000"), n("-"+fee)).String() || gasTotal != "-"+fee {
		t.Fatalf("chain %s, gas_expense %s", chainTotal, gasTotal)
	}
	// Both entries of the transaction, summing to zero; a user's balance is untouched.
	var sum string
	var entries int
	_ = s.pool.QueryRow(ctx, `SELECT sum(amount)::text, count(*) FROM entries WHERE ledger_transaction_id = $1`, res.TransactionID).Scan(&sum, &entries)
	if sum != "0" || entries != 2 {
		t.Fatalf("sum %s over %d entries", sum, entries)
	}
	bal(t, s, "alice", "1000", "0", "1000")
	var src, purpose, hash string
	_ = s.pool.QueryRow(ctx, `SELECT source_type, purpose, tx_hash FROM ledger_transactions WHERE id = $1`, res.TransactionID).Scan(&src, &purpose, &hash)
	if src != "gas_fee" || purpose != "withdrawal" || hash != gasTx {
		t.Fatalf("%s %s %s", src, purpose, hash)
	}
}

func TestGasFee_ReplayIsANoOpAndDifferentValuesAre409(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first, err := s.PostGasFee(ctx, gasFee(gasTx, 21000, "7", "gas_topup"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.PostGasFee(ctx, gasFee("0x"+upper(gasTx[2:]), 21000, "7", "gas_topup")) // hash casing is not a different tx
	if err != nil || again.TransactionID != first.TransactionID || again.Amount.Cmp(first.Amount) != 0 {
		t.Fatalf("replay = %+v %v, want %+v", again, err, first)
	}
	if _, gas := nativeTotals(t, s); gas != "-147000" {
		t.Fatalf("gas_expense %s after a replay, want -147000", gas)
	}
	for name, g := range map[string]GasFee{
		"other gas_used": gasFee(gasTx, 21001, "7", "gas_topup"),
		"other price":    gasFee(gasTx, 21000, "8", "gas_topup"),
		"other purpose":  gasFee(gasTx, 21000, "7", "sweep"),
	} {
		if _, err := s.PostGasFee(ctx, g); !errors.Is(err, ErrIdempotencyKeyReused) {
			t.Errorf("%s: %v, want ErrIdempotencyKeyReused", name, err)
		}
	}
	if count(t, s, `SELECT count(*) FROM ledger_transactions WHERE source_type = 'gas_fee'`) != 1 {
		t.Fatal("a gas fee was posted twice")
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestGasFee_ConcurrentReportsOfOneTxPostOnce(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.PostGasFee(context.Background(), gasFee(gasTx, 100, "3", "sweep")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, gas := nativeTotals(t, s); gas != "-300" {
		t.Fatalf("gas_expense %s, want -300 (once)", gas)
	}
}

func TestGasFee_Validation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for name, g := range map[string]GasFee{
		"unknown purpose": gasFee(gasTx, 1, "1", "tip"),
		"no purpose":      gasFee(gasTx, 1, "1", ""),
		"bad hash":        gasFee("0x12", 1, "1", "sweep"),
		"zero gas":        gasFee(gasTx, 0, "1", "sweep"),
		"negative price":  {ChainID: 1, TxHash: gasTx, GasUsed: 1, EffectiveGasPrice: big.NewInt(-1), Purpose: "sweep"},
		"no chain":        {ChainID: 0, TxHash: gasTx, GasUsed: 1, EffectiveGasPrice: big.NewInt(1), Purpose: "sweep"},
	} {
		if _, err := s.PostGasFee(ctx, g); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if count(t, s, `SELECT count(*) FROM ledger_transactions WHERE source_type = 'gas_fee'`) != 0 {
		t.Fatal("something was posted")
	}
}

// A zero fee (a zero-price test chain) has nothing to post and must not loop a caller that retries until 200.
func TestGasFee_ZeroFeePostsNothing(t *testing.T) {
	s := newStore(t)
	res, err := s.PostGasFee(context.Background(), gasFee(gasTx, 21000, "0", "withdrawal"))
	if err != nil || res.TransactionID != "" || res.Amount.Sign() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if count(t, s, `SELECT count(*) FROM ledger_transactions WHERE source_type = 'gas_fee'`) != 0 {
		t.Fatal("a zero fee posted a transaction")
	}
}

// The Gas Expense Account may go negative without bound; a user's Available Balance still may not.
func TestGasFee_GasExpenseGoesNegativeButUsersStillCannot(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.PostGasFee(ctx, gasFee(gasTx, 1, "5", "withdrawal")); err != nil {
		t.Fatalf("gas_expense must be allowed below zero: %v", err)
	}
	mustApply(t, s, deposit("d1", "bob", "10"))
	if _, err := s.Apply(ctx, Transaction{IdempotencyKey: "w", Source: SourceDeposit, Deposit: &DepositRef{ChainID: 1, TxHash: gasTx, LogIndex: 9},
		Entries: []Entry{user("bob", eth, "-11"), chain(eth, "11")}}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a user overdraft: %v", err)
	}
}

// Only a native asset has a Gas Expense Account, and every native asset has exactly one.
func TestGasFee_GasExpenseAccountsExistOnlyForNativeAssets(t *testing.T) {
	s := newStore(t)
	if got := count(t, s, `SELECT count(*) FROM accounts WHERE kind = 'gas_expense'`); got != 1 {
		t.Fatalf("%d gas_expense accounts, want 1 (ETH only)", got)
	}
	if err := s.SyncAssets(context.Background(), registry()); err != nil { // a second start changes nothing
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM accounts WHERE kind = 'gas_expense'`); got != 1 {
		t.Fatalf("%d gas_expense accounts after a resync", got)
	}
}
