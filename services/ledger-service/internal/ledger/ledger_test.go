package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"digiwal/services/ledger-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	eth = "11111111-1111-1111-1111-111111111111"
	tok = "22222222-2222-2222-2222-222222222222"
)

func n(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }

func registry() []Asset {
	return []Asset{
		{ID: eth, Type: AssetNative, Symbol: "ETH", Decimals: 18, HotFloor: n("30"), HotCeiling: n("120"), MinWithdrawalAmount: n("1")},
		{ID: tok, Type: AssetERC20, ContractAddress: "0x5FbDB2315678afecb367f032d93F642f64180aa3", Symbol: "TT", Decimals: 18, HotFloor: n("0"), HotCeiling: n("0"), MinWithdrawalAmount: n("1")},
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.SyncAssets(context.Background(), registry()); err != nil {
		t.Fatal(err)
	}
	return s
}

func user(id, asset string, amt string) Entry {
	return Entry{Ref: RefUser, UserID: id, AssetID: asset, Amount: n(amt)}
}
func chain(asset string, amt string) Entry {
	return Entry{Ref: RefChain, AssetID: asset, Amount: n(amt)}
}

func deposit(key, uid string, amt string) Transaction {
	return Transaction{IdempotencyKey: key, Source: SourceDeposit,
		Deposit: &DepositRef{ChainID: 1, TxHash: "0x" + fmt.Sprintf("%064x", new(big.Int).SetBytes([]byte(key))), LogIndex: -1},
		Entries: []Entry{user(uid, eth, amt), chain(eth, "-"+amt)}}
}

func withdrawal(key, uid string, amt string) Transaction {
	return Transaction{IdempotencyKey: key, Source: SourceWithdrawal,
		Entries: []Entry{user(uid, eth, "-"+amt), chain(eth, amt)}}
}

func mustApply(t *testing.T, s *Store, tx Transaction) {
	t.Helper()
	if st, err := s.Apply(context.Background(), tx); err != nil || st != Applied {
		t.Fatalf("apply %s: %v %v", tx.IdempotencyKey, st, err)
	}
}

// ---- invariant #2, unit level (no database) -------------------------------

func TestValidate_SumToZero(t *testing.T) {
	ok := Transaction{IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "-5"), chain(eth, "5")}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Transaction{
		"credit only":    {IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "5"), chain(eth, "4")}},
		"debit only":     {IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "-5"), chain(eth, "4")}},
		"one off by one": {IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "-5"), chain(eth, "6")}},
		// +5 ETH and -5 of another asset cancel numerically but are not balanced.
		"cross-asset cheat": {IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "5"), chain(tok, "-5")}},
	}
	for name, tx := range cases {
		if err := tx.Validate(); !errors.Is(err, ErrUnbalanced) {
			t.Errorf("%s: got %v, want ErrUnbalanced", name, err)
		}
	}
	// three entries that do balance
	three := Transaction{IdempotencyKey: "k", Source: SourceWithdrawal, Entries: []Entry{user("a", eth, "-5"), user("b", eth, "2"), chain(eth, "3")}}
	if err := three.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_Shape(t *testing.T) {
	good := func() Transaction { return withdrawal("k", "a", "5") }
	mut := func(f func(*Transaction)) Transaction { tx := good(); f(&tx); return tx }
	cases := map[string]Transaction{
		"no key":            mut(func(x *Transaction) { x.IdempotencyKey = "" }),
		"bad source":        mut(func(x *Transaction) { x.Source = "gift" }),
		"one entry":         mut(func(x *Transaction) { x.Entries = x.Entries[:1] }),
		"zero amount":       mut(func(x *Transaction) { x.Entries[0].Amount = big.NewInt(0); x.Entries[1].Amount = big.NewInt(0) }),
		"nil amount":        mut(func(x *Transaction) { x.Entries[0].Amount = nil }),
		"user without id":   mut(func(x *Transaction) { x.Entries[0].UserID = "" }),
		"chain with user":   mut(func(x *Transaction) { x.Entries[1].UserID = "a" }),
		"bad ref":           mut(func(x *Transaction) { x.Entries[0].Ref = "bank" }),
		"bad asset id":      mut(func(x *Transaction) { x.Entries[0].AssetID = "eth" }),
		"deposit no ref":    mut(func(x *Transaction) { x.Source = SourceDeposit }),
		"withdrawal w/ ref": mut(func(x *Transaction) { x.Deposit = &DepositRef{} }),
	}
	for name, tx := range cases {
		if err := tx.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// ---- invariants #1-#4 against a real Postgres -----------------------------

func TestApply_DepositThenWithdrawalMovesBalances(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "1000" {
		t.Fatalf("balance %s", b)
	}
	mustApply(t, s, withdrawal("w1", "alice", "400"))
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "600" {
		t.Fatalf("balance %s", b)
	}
	// the Chain Account mirrors -(net funds users hold): reconcilable against custody
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-600" {
		t.Fatalf("chain balance %s, want -600", c)
	}
}

func TestApply_OverdraftIsRejectedAndLeavesNothingBehind(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))

	if _, err := s.Apply(ctx, withdrawal("w-too-much", "alice", "101")); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "100" {
		t.Fatalf("balance changed to %s", b)
	}
	// fully rolled back: no ledger transaction, so the same key can be retried later
	var txs, entries int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE idempotency_key = 'w-too-much'`).Scan(&txs)
	s.pool.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&entries)
	if txs != 0 || entries != 2 { // only the deposit's two entries
		t.Fatalf("leftovers after rollback: txs=%d entries=%d", txs, entries)
	}
	mustApply(t, s, deposit("d2", "alice", "50"))
	mustApply(t, s, withdrawal("w-too-much", "alice", "101")) // now affordable, same key works
}

func TestApply_FailedTransactionDoesNotCreateAnAccount(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	if _, err := s.Apply(ctx, withdrawal("w", "ghost", "1")); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	var accts int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE user_id = 'ghost'`).Scan(&accts)
	if accts != 0 {
		t.Fatal("a rolled-back transaction must not leave an account behind")
	}
}

// Two concurrent withdrawals racing one balance: exactly enough succeed, never an overdraft.
func TestApply_ConcurrentWithdrawalsNeverOverdraw(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))

	const attempts = 50 // each takes 100 -> only 10 can succeed
	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := s.Apply(ctx, withdrawal(fmt.Sprintf("w%d", i), "alice", "100")); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 || insufficient.Load() != attempts-10 {
		t.Fatalf("ok=%d insufficient=%d, want 10/%d", ok.Load(), insufficient.Load(), attempts-10)
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.Sign() != 0 {
		t.Fatalf("balance %s, want exactly 0 (never negative)", b)
	}
}

func TestApply_IdempotentReplay(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tx := deposit("d1", "alice", "1000")
	mustApply(t, s, tx)
	for i := 0; i < 3; i++ {
		if st, err := s.Apply(ctx, tx); err != nil || st != AlreadyApplied {
			t.Fatalf("replay %d: %v %v", i, st, err)
		}
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "1000" {
		t.Fatalf("replay changed the balance to %s", b)
	}
	var entries int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&entries)
	if entries != 2 {
		t.Fatalf("%d entries, want 2", entries)
	}
}

func TestApply_ConcurrentReplayOfOneEventCreditsOnce(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tx := deposit("d1", "alice", "1000")
	var applied, already atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.Apply(ctx, tx)
			if err != nil {
				t.Error(err)
				return
			}
			if st == Applied {
				applied.Add(1)
			} else {
				already.Add(1)
			}
		}()
	}
	wg.Wait()
	if applied.Load() != 1 || already.Load() != 29 {
		t.Fatalf("applied=%d already=%d, want 1/29", applied.Load(), already.Load())
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "1000" {
		t.Fatalf("double credit: %s", b)
	}
}

func TestApply_KeyReuseWithDifferentContentIsRejected(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	if _, err := s.Apply(ctx, deposit("d1", "alice", "999")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different amount: %v", err)
	}
	if _, err := s.Apply(ctx, deposit("d1", "bob", "1000")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different user: %v", err)
	}
	// the same on-chain event under a different key is also refused
	other := deposit("different-key", "alice", "1000")
	other.Deposit = deposit("d1", "alice", "1000").Deposit
	if _, err := s.Apply(ctx, other); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same natural key, new idempotency key: %v", err)
	}
}

func TestApply_TwoLogsInOneTxAreTwoDeposits(t *testing.T) {
	s := newStore(t)
	a, b := deposit("d-log-1", "alice", "10"), deposit("d-log-2", "alice", "10")
	a.Deposit = &DepositRef{ChainID: 1, TxHash: "0x" + fmt.Sprintf("%064x", 7), LogIndex: 0}
	b.Deposit = &DepositRef{ChainID: 1, TxHash: "0x" + fmt.Sprintf("%064x", 7), LogIndex: 1}
	mustApply(t, s, a)
	mustApply(t, s, b)
	if bal, _ := s.Balance(context.Background(), "alice", eth); bal.String() != "20" {
		t.Fatalf("balance %s", bal)
	}
}

func TestApply_UnknownOrRemovedAssetIsRefused(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	ghost := "99999999-9999-9999-9999-999999999999"
	tx := Transaction{IdempotencyKey: "d", Source: SourceDeposit, Deposit: &DepositRef{ChainID: 1, TxHash: "0xaa", LogIndex: -1},
		Entries: []Entry{user("a", ghost, "5"), chain(ghost, "-5")}}
	if _, err := s.Apply(ctx, tx); !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("unregistered asset: %v", err)
	}
	// drop the token from the registry: it is deactivated, not deleted
	if err := s.SyncAssets(ctx, registry()[:1]); err != nil {
		t.Fatal(err)
	}
	tok := Transaction{IdempotencyKey: "d2", Source: SourceDeposit, Deposit: &DepositRef{ChainID: 1, TxHash: "0xbb", LogIndex: 0},
		Entries: []Entry{user("a", "22222222-2222-2222-2222-222222222222", "5"), chain("22222222-2222-2222-2222-222222222222", "-5")}}
	if _, err := s.Apply(ctx, tok); !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("deactivated asset: %v", err)
	}
}

func TestApply_AssetsAreIndependent(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	tk := Transaction{IdempotencyKey: "w-tok", Source: SourceWithdrawal, Entries: []Entry{user("alice", "22222222-2222-2222-2222-222222222222", "-1"), chain("22222222-2222-2222-2222-222222222222", "1")}}
	if _, err := s.Apply(ctx, tk); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("ETH balance must not fund a token withdrawal: %v", err)
	}
}

func TestApply_MultiUserTransactionIsAtomic(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	// alice -50, bob +50 works; a transfer that overdraws alice moves nothing for bob either
	mustApply(t, s, Transaction{IdempotencyKey: "t1", Source: SourceCorrection, Entries: []Entry{user("alice", eth, "-50"), user("bob", eth, "50")}})
	if _, err := s.Apply(ctx, Transaction{IdempotencyKey: "t2", Source: SourceCorrection, Entries: []Entry{user("alice", eth, "-51"), user("bob", eth, "51")}}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	a, _ := s.Balance(ctx, "alice", eth)
	b, _ := s.Balance(ctx, "bob", eth)
	if a.String() != "50" || b.String() != "50" {
		t.Fatalf("alice=%s bob=%s", a, b)
	}
}

func TestApply_AmountsBeyondInt64AreExact(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	huge := "123456789012345678901234567890" // ~1.2e29 wei; overflows a bigint column
	mustApply(t, s, deposit("d1", "alice", huge))
	mustApply(t, s, withdrawal("w1", "alice", "1"))
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "123456789012345678901234567889" {
		t.Fatalf("balance %s", b)
	}
}

// ---- invariant #4: entries are immutable ----------------------------------

func TestEntriesAreAppendOnly(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	for name, q := range map[string]string{
		"update":   `UPDATE entries SET amount = 1`,
		"delete":   `DELETE FROM entries`,
		"truncate": `TRUNCATE entries CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s on entries succeeded; entries must be immutable", name)
		}
	}
}

// ---- Asset Registry ---------------------------------------------------------

func TestSyncAssets_UpsertsCreatesChainAccountsAndDeactivates(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	// syncing again is idempotent and picks up edits
	r := registry()
	r[0].HotFloor = n("77")
	if err := s.SyncAssets(ctx, r); err != nil {
		t.Fatal(err)
	}
	var floor string
	s.pool.QueryRow(ctx, `SELECT hot_floor::text FROM assets WHERE id = $1`, eth).Scan(&floor)
	if floor != "77" {
		t.Fatalf("hot_floor %s, want 77", floor)
	}
	var chains int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE kind = 'chain'`).Scan(&chains)
	if chains != 2 {
		t.Fatalf("%d chain accounts, want one per asset (2)", chains)
	}
	// removing an asset deactivates it; putting it back reactivates it
	if err := s.SyncAssets(ctx, r[:1]); err != nil {
		t.Fatal(err)
	}
	var active bool
	s.pool.QueryRow(ctx, `SELECT active FROM assets WHERE id = $1`, tok).Scan(&active)
	if active {
		t.Fatal("removed asset should be inactive")
	}
	if err := s.SyncAssets(ctx, r); err != nil {
		t.Fatal(err)
	}
	s.pool.QueryRow(ctx, `SELECT active FROM assets WHERE id = $1`, tok).Scan(&active)
	if !active {
		t.Fatal("re-listed asset should be active again")
	}
}

func TestMigrationDownUp(t *testing.T) {
	dsn := testdb.New(t)
	m, err := NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
