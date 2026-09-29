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

func mustApply(t *testing.T, s *Store, tx Transaction) {
	t.Helper()
	if st, err := s.Apply(context.Background(), tx); err != nil || st != Applied {
		t.Fatalf("apply %s: %v %v", tx.IdempotencyKey, st, err)
	}
}

func lockReq(key, uid, amt string) LockRequest {
	return LockRequest{IdempotencyKey: key, UserID: uid, AssetID: eth, Amount: n(amt)}
}

func mustLock(t *testing.T, s *Store, key, uid, amt string) {
	t.Helper()
	if l, err := s.Lock(context.Background(), lockReq(key, uid, amt)); err != nil || l.Status != LockActive {
		t.Fatalf("lock %s: %+v %v", key, l, err)
	}
}

func mustSettle(t *testing.T, s *Store, key string) SettleResult {
	t.Helper()
	r, err := s.Settle(context.Background(), key)
	if err != nil {
		t.Fatalf("settle %s: %v", key, err)
	}
	return r
}

// bal asserts total/locked/available for alice-style users.
func bal(t *testing.T, s *Store, uid, total, locked, available string) {
	t.Helper()
	b, err := s.BalancesFor(context.Background(), uid, eth)
	if err != nil {
		t.Fatal(err)
	}
	if b.Total.String() != total || b.Locked.String() != locked || b.Available.String() != available {
		t.Fatalf("%s balances total/locked/available = %s/%s/%s, want %s/%s/%s", uid, b.Total, b.Locked, b.Available, total, locked, available)
	}
}

func count(t *testing.T, s *Store, q string, args ...any) (c int) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), q, args...).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// entryIDs returns the ids of every entry a ledger transaction posted.
func entryIDs(t *testing.T, s *Store, key string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT e.id FROM entries e JOIN ledger_transactions l ON l.id = e.ledger_transaction_id WHERE l.idempotency_key = $1 ORDER BY e.amount`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func correction(key string, ids ...string) Transaction {
	tx := Transaction{IdempotencyKey: key, Source: SourceCorrection}
	for _, id := range ids {
		tx.Entries = append(tx.Entries, Entry{CorrectsEntryID: id})
	}
	return tx
}

// ---- invariant #2, unit level (no database) -------------------------------

func TestValidate_SumToZero(t *testing.T) {
	if err := deposit("k", "a", "5").Validate(); err != nil {
		t.Fatal(err)
	}
	dep := func(e ...Entry) Transaction {
		tx := deposit("k", "a", "5")
		tx.Entries = e
		return tx
	}
	cases := map[string]Transaction{
		"credit only":    dep(user("a", eth, "5"), chain(eth, "-4")),
		"debit only":     dep(user("a", eth, "-5"), chain(eth, "4")),
		"one off by one": dep(user("a", eth, "5"), chain(eth, "-6")),
		// +5 ETH and -5 of another asset cancel numerically but are not balanced.
		"cross-asset cheat": dep(user("a", eth, "5"), chain(tok, "-5")),
	}
	for name, tx := range cases {
		if err := tx.Validate(); !errors.Is(err, ErrUnbalanced) {
			t.Errorf("%s: got %v, want ErrUnbalanced", name, err)
		}
	}
	if err := dep(user("a", eth, "5"), user("b", eth, "2"), chain(eth, "-7")).Validate(); err != nil {
		t.Fatalf("three balanced entries: %v", err)
	}
}

func TestValidate_Shape(t *testing.T) {
	mut := func(f func(*Transaction)) Transaction { tx := deposit("k", "a", "5"); f(&tx); return tx }
	cases := map[string]Transaction{
		"no key":                    mut(func(x *Transaction) { x.IdempotencyKey = "" }),
		"bad source":                mut(func(x *Transaction) { x.Source = "gift" }),
		"settle is not apply":       mut(func(x *Transaction) { x.Source = SourceWithdrawalSettle; x.Deposit = nil }),
		"one entry":                 mut(func(x *Transaction) { x.Entries = x.Entries[:1] }),
		"zero amount":               mut(func(x *Transaction) { x.Entries[0].Amount = big.NewInt(0); x.Entries[1].Amount = big.NewInt(0) }),
		"nil amount":                mut(func(x *Transaction) { x.Entries[0].Amount = nil }),
		"user without id":           mut(func(x *Transaction) { x.Entries[0].UserID = "" }),
		"chain with user":           mut(func(x *Transaction) { x.Entries[1].UserID = "a" }),
		"bad ref":                   mut(func(x *Transaction) { x.Entries[0].Ref = "bank" }),
		"bad asset id":              mut(func(x *Transaction) { x.Entries[0].AssetID = "eth" }),
		"deposit without ref":       mut(func(x *Transaction) { x.Deposit = nil }),
		"correction w/ deposit ref": mut(func(x *Transaction) { x.Source = SourceCorrection }),
		"corrects on a deposit":     mut(func(x *Transaction) { x.Entries[0].CorrectsEntryID = "33333333-3333-3333-3333-333333333333" }),
	}
	for name, tx := range cases {
		if err := tx.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	// a correction must name what it corrects; it needs nothing else
	noRef := Transaction{IdempotencyKey: "k", Source: SourceCorrection, Entries: []Entry{{}, {}}}
	if err := noRef.Validate(); !errors.Is(err, ErrInvalidCorrection) {
		t.Errorf("correction without corrects_entry_id: %v", err)
	}
	ok := correction("k", "33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444")
	if err := ok.Validate(); err != nil {
		t.Errorf("valid correction: %v", err)
	}
}

func TestLockRequestValidation(t *testing.T) {
	s := newStore(t)
	mut := func(f func(*LockRequest)) LockRequest { r := lockReq("k", "a", "5"); f(&r); return r }
	for name, r := range map[string]LockRequest{
		"no key":     mut(func(r *LockRequest) { r.IdempotencyKey = "" }),
		"no user":    mut(func(r *LockRequest) { r.UserID = "" }),
		"bad asset":  mut(func(r *LockRequest) { r.AssetID = "eth" }),
		"zero":       mut(func(r *LockRequest) { r.Amount = big.NewInt(0) }),
		"negative":   mut(func(r *LockRequest) { r.Amount = big.NewInt(-1) }),
		"nil amount": mut(func(r *LockRequest) { r.Amount = nil }),
	} {
		if _, err := s.Lock(context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.Lock(context.Background(), LockRequest{IdempotencyKey: "k", UserID: "a", AssetID: "99999999-9999-9999-9999-999999999999", Amount: n("1")}); !errors.Is(err, ErrUnknownAsset) {
		t.Errorf("unregistered asset: %v", err)
	}
}

// ---- Apply: deposits (invariants #2, #3, #5) -----------------------------

func TestDeposit_CreditsAndMirrorsOnTheChainAccount(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	bal(t, s, "alice", "1000", "0", "1000")
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-1000" {
		t.Fatalf("chain balance %s, want -1000 (it goes negative by design)", c)
	}
}

func TestDeposit_IdempotentReplay(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tx := deposit("d1", "alice", "1000")
	mustApply(t, s, tx)
	for i := 0; i < 3; i++ {
		if st, err := s.Apply(ctx, tx); err != nil || st != AlreadyApplied {
			t.Fatalf("replay %d: %v %v", i, st, err)
		}
	}
	bal(t, s, "alice", "1000", "0", "1000")
	if e := count(t, s, `SELECT count(*) FROM entries`); e != 2 {
		t.Fatalf("%d entries, want 2", e)
	}
}

func TestDeposit_ConcurrentReplayOfOneEventCreditsOnce(t *testing.T) {
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
	bal(t, s, "alice", "1000", "0", "1000")
}

func TestDeposit_KeyReuseWithDifferentContentIsRejected(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	if _, err := s.Apply(ctx, deposit("d1", "alice", "999")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different amount: %v", err)
	}
	if _, err := s.Apply(ctx, deposit("d1", "bob", "1000")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different user: %v", err)
	}
	other := deposit("different-key", "alice", "1000") // the same on-chain event under a new key
	other.Deposit = deposit("d1", "alice", "1000").Deposit
	if _, err := s.Apply(ctx, other); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same natural key, new idempotency key: %v", err)
	}
}

func TestDeposit_TwoLogsInOneTxAreTwoDeposits(t *testing.T) {
	s := newStore(t)
	a, b := deposit("d-log-1", "alice", "10"), deposit("d-log-2", "alice", "10")
	a.Deposit = &DepositRef{ChainID: 1, TxHash: "0x" + fmt.Sprintf("%064x", 7), LogIndex: 0}
	b.Deposit = &DepositRef{ChainID: 1, TxHash: "0x" + fmt.Sprintf("%064x", 7), LogIndex: 1}
	mustApply(t, s, a)
	mustApply(t, s, b)
	bal(t, s, "alice", "20", "0", "20")
}

func TestDeposit_UnknownOrRemovedAssetIsRefused(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	ghost := "99999999-9999-9999-9999-999999999999"
	tx := Transaction{IdempotencyKey: "d", Source: SourceDeposit, Deposit: &DepositRef{ChainID: 1, TxHash: "0xaa", LogIndex: -1},
		Entries: []Entry{user("a", ghost, "5"), chain(ghost, "-5")}}
	if _, err := s.Apply(ctx, tx); !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("unregistered asset: %v", err)
	}
	if err := s.SyncAssets(ctx, registry()[:1]); err != nil { // token leaves the registry: deactivated, not deleted
		t.Fatal(err)
	}
	dt := Transaction{IdempotencyKey: "d2", Source: SourceDeposit, Deposit: &DepositRef{ChainID: 1, TxHash: "0xbb", LogIndex: 0},
		Entries: []Entry{user("a", tok, "5"), chain(tok, "-5")}}
	if _, err := s.Apply(ctx, dt); !errors.Is(err, ErrUnknownAsset) {
		t.Fatalf("deactivated asset: %v", err)
	}
}

func TestDeposit_AmountsBeyondInt64AreExact(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "123456789012345678901234567890")) // ~1.2e29 wei
	mustLock(t, s, "w1", "alice", "1")
	mustSettle(t, s, "w1")
	bal(t, s, "alice", "123456789012345678901234567889", "0", "123456789012345678901234567889")
}

// ---- Lock (invariants #1, #6, #7) ----------------------------------------

func TestLock_ReservesCapacityWithoutMovingMoney(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "1000"))
	entriesBefore := count(t, s, `SELECT count(*) FROM entries`)

	mustLock(t, s, "w1", "alice", "400")
	bal(t, s, "alice", "1000", "400", "600") // total untouched, only locked moved
	if got := count(t, s, `SELECT count(*) FROM entries`); got != entriesBefore {
		t.Fatalf("a Lock posted %d entries, want none", got-entriesBefore)
	}
	if c, _ := s.ChainBalance(context.Background(), eth); c.String() != "-1000" {
		t.Fatalf("a Lock moved the chain account to %s", c)
	}
}

func TestLock_CountsAgainstAvailableNotTotal(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	mustLock(t, s, "w1", "alice", "60")
	// 100 total, but only 40 available: a second 60 must be refused
	if _, err := s.Lock(ctx, lockReq("w2", "alice", "60")); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	mustLock(t, s, "w3", "alice", "40") // exactly what's left is fine
	bal(t, s, "alice", "100", "100", "0")
}

func TestLock_InsufficientFundsLeavesNothingBehind(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	if _, err := s.Lock(ctx, lockReq("w-big", "alice", "101")); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM locks`); got != 0 {
		t.Fatalf("%d lock rows after a refused lock", got)
	}
	bal(t, s, "alice", "100", "0", "100")
	// the key was never consumed: once funded, the same key works
	mustApply(t, s, deposit("d2", "alice", "50"))
	mustLock(t, s, "w-big", "alice", "101")

	// and a user who never deposited gets no account out of a refused lock
	if _, err := s.Lock(ctx, lockReq("w-ghost", "ghost", "1")); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM accounts WHERE user_id = 'ghost'`); got != 0 {
		t.Fatal("a rolled-back lock must not leave an account behind")
	}
}

// Many concurrent locks racing one balance: exactly enough succeed, never an over-reservation.
func TestLock_ConcurrentLocksNeverOverReserve(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))

	const attempts = 50 // each holds 100 -> only 10 fit
	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := s.Lock(ctx, lockReq(fmt.Sprintf("w%d", i), "alice", "100")); {
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
	bal(t, s, "alice", "1000", "1000", "0")
}

func TestLock_IdempotentReplayReturnsCurrentStatus(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	for i := 0; i < 3; i++ {
		l, err := s.Lock(ctx, lockReq("w1", "alice", "400"))
		if err != nil || l.Status != LockActive {
			t.Fatalf("replay: %+v %v", l, err)
		}
	}
	bal(t, s, "alice", "1000", "400", "600") // never double-locked

	mustSettle(t, s, "w1")
	l, err := s.Lock(ctx, lockReq("w1", "alice", "400")) // a very late retry
	if err != nil || l.Status != LockSettled {
		t.Fatalf("late replay: %+v %v, want the settled status back", l, err)
	}
	bal(t, s, "alice", "600", "0", "600")
}

func TestLock_ConcurrentReplayLocksOnce(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Lock(ctx, lockReq("w1", "alice", "300")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	bal(t, s, "alice", "1000", "300", "700")
	if got := count(t, s, `SELECT count(*) FROM locks`); got != 1 {
		t.Fatalf("%d lock rows, want 1", got)
	}
}

func TestLock_KeyReuseWithDifferentRequestIsRejected(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustApply(t, s, deposit("d2", "bob", "1000"))
	mustLock(t, s, "w1", "alice", "100")
	if _, err := s.Lock(ctx, lockReq("w1", "alice", "101")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different amount: %v", err)
	}
	if _, err := s.Lock(ctx, lockReq("w1", "bob", "100")); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different user: %v", err)
	}
	bal(t, s, "alice", "1000", "100", "900")
	bal(t, s, "bob", "1000", "0", "1000")
}

func TestLock_AssetsAreIndependent(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	if _, err := s.Lock(ctx, LockRequest{IdempotencyKey: "w", UserID: "alice", AssetID: tok, Amount: n("1")}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("an ETH balance must not fund a token lock: %v", err)
	}
}

// ---- Settle (invariants #7, #8; atomicity) -------------------------------

func TestSettle_PostsTheRealDebitForTheFirstTime(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	res := mustSettle(t, s, "w1")

	bal(t, s, "alice", "600", "0", "600") // total and locked both dropped by exactly the lock amount
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-600" {
		t.Fatalf("chain balance %s, want -600", c)
	}
	// exactly two entries, opposite and equal, under a withdrawal_settle transaction keyed by the Lock's key
	var src, key string
	var sum string
	s.pool.QueryRow(ctx, `SELECT source_type, idempotency_key FROM ledger_transactions WHERE id = $1`, res.LedgerTransactionID).Scan(&src, &key)
	s.pool.QueryRow(ctx, `SELECT sum(amount)::text FROM entries WHERE ledger_transaction_id = $1`, res.LedgerTransactionID).Scan(&sum)
	if src != "withdrawal_settle" || key != "w1" || sum != "0" {
		t.Fatalf("settle tx: source=%s key=%s entries sum=%s", src, key, sum)
	}
	if got := count(t, s, `SELECT count(*) FROM entries WHERE ledger_transaction_id = $1`, res.LedgerTransactionID); got != 2 {
		t.Fatalf("%d entries, want 2", got)
	}
}

func TestSettle_IsIdempotentAndNeverDoubleDebits(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	first := mustSettle(t, s, "w1")
	for i := 0; i < 3; i++ {
		if again := mustSettle(t, s, "w1"); again != first {
			t.Fatalf("replay %+v != first %+v", again, first)
		}
	}
	bal(t, s, "alice", "600", "0", "600")
	if got := count(t, s, `SELECT count(*) FROM ledger_transactions WHERE source_type = 'withdrawal_settle'`); got != 1 {
		t.Fatalf("%d settle transactions, want 1", got)
	}
}

func TestSettle_ConcurrentSettlesDebitOnce(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Settle(context.Background(), "w1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	bal(t, s, "alice", "600", "0", "600")
	if got := count(t, s, `SELECT count(*) FROM entries e JOIN ledger_transactions l ON l.id = e.ledger_transaction_id WHERE l.source_type = 'withdrawal_settle'`); got != 2 {
		t.Fatalf("%d settle entries, want 2", got)
	}
}

func TestSettle_UnknownLockAndWrongResolution(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	if _, err := s.Settle(ctx, "never-locked"); !errors.Is(err, ErrLockNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	mustLock(t, s, "w1", "alice", "100")
	if err := s.Release(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	// a released lock must never read as "debited": settle refuses with a conflict
	if _, err := s.Settle(ctx, "w1"); !errors.Is(err, ErrLockAlreadyReleased) {
		t.Fatalf("settle after release: %v", err)
	}
	bal(t, s, "alice", "1000", "0", "1000")
}

// A crash in the middle of Settle must not leave a lock 'settled' with no Entry
// (the bug an earlier design draft had): the whole thing is one transaction.
func TestSettle_IsAtomicUnderAMidTransactionFailure(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "777")

	// make the entries insert (the last step of Settle) blow up
	for _, q := range []string{
		`CREATE FUNCTION boom() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'simulated crash'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER boom BEFORE INSERT ON entries FOR EACH ROW WHEN (NEW.amount = 777) EXECUTE FUNCTION boom()`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Settle(ctx, "w1"); err == nil {
		t.Fatal("settle should have failed")
	}
	var status string
	s.pool.QueryRow(ctx, `SELECT status FROM locks WHERE idempotency_key = 'w1'`).Scan(&status)
	if status != "active" {
		t.Fatalf("lock is %q after a failed settle; the status flip must roll back with everything else", status)
	}
	bal(t, s, "alice", "1000", "777", "223")
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-1000" {
		t.Fatalf("chain balance %s changed", c)
	}
	if got := count(t, s, `SELECT count(*) FROM ledger_transactions WHERE source_type = 'withdrawal_settle'`); got != 0 {
		t.Fatal("a settle transaction row survived the failure")
	}

	// once the fault is gone the same lock settles normally
	s.pool.Exec(ctx, `DROP TRIGGER boom ON entries`)
	mustSettle(t, s, "w1")
	bal(t, s, "alice", "223", "0", "223")
}

// ---- Release (invariant #8) ----------------------------------------------

func TestRelease_ReturnsCapacityAndPostsNothing(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	entries := count(t, s, `SELECT count(*) FROM entries`)
	txs := count(t, s, `SELECT count(*) FROM ledger_transactions`)

	if err := s.Release(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	bal(t, s, "alice", "1000", "0", "1000")
	if count(t, s, `SELECT count(*) FROM entries`) != entries || count(t, s, `SELECT count(*) FROM ledger_transactions`) != txs {
		t.Fatal("Release posted an entry or a ledger transaction; there is nothing to compensate")
	}
	if err := s.Release(ctx, "w1"); err != nil { // idempotent
		t.Fatalf("second release: %v", err)
	}
	bal(t, s, "alice", "1000", "0", "1000")
}

func TestRelease_UnknownAndAlreadySettled(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	if err := s.Release(ctx, "nope"); !errors.Is(err, ErrLockNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	mustLock(t, s, "w1", "alice", "100")
	mustSettle(t, s, "w1")
	if err := s.Release(ctx, "w1"); !errors.Is(err, ErrLockAlreadySettled) {
		t.Fatalf("release after settle: %v", err)
	}
	bal(t, s, "alice", "900", "0", "900") // money really moved; release must not give it back
}

// Invariant #7: settled XOR released, never both, whichever caller arrives first.
func TestLock_SettleAndReleaseRaceResolvesExactlyOnce(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100000"))
	const locks = 30
	for i := 0; i < locks; i++ {
		mustLock(t, s, fmt.Sprintf("w%d", i), "alice", "100")
	}
	var settled, released atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < locks; i++ {
		key := fmt.Sprintf("w%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.Settle(ctx, key); err == nil {
				settled.Add(1)
			} else if !errors.Is(err, ErrLockAlreadyReleased) {
				t.Errorf("settle %s: %v", key, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.Release(ctx, key); err == nil {
				released.Add(1)
			} else if !errors.Is(err, ErrLockAlreadySettled) {
				t.Errorf("release %s: %v", key, err)
			}
		}()
	}
	wg.Wait()
	if settled.Load()+released.Load() != locks {
		t.Fatalf("settled=%d released=%d: each lock must resolve exactly one way (want %d total)", settled.Load(), released.Load(), locks)
	}
	// the books agree with however the races fell
	debited := int64(settled.Load()) * 100
	bal(t, s, "alice", fmt.Sprint(100000-debited), "0", fmt.Sprint(100000-debited))
	if c, _ := s.ChainBalance(ctx, eth); c.String() != fmt.Sprint(-(100000 - debited)) {
		t.Fatalf("chain balance %s", c)
	}
}

// ---- Corrections (invariants #9, #11) ------------------------------------

func TestCorrection_ReversesASettleExactly(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	mustSettle(t, s, "w1")
	bal(t, s, "alice", "600", "0", "600")

	ids := entryIDs(t, s, "w1")
	if len(ids) != 2 {
		t.Fatalf("settle has %d entries", len(ids))
	}
	// the caller supplies only which entries to reverse; the amounts it sends are ignored
	tx := correction("w1:refund", ids...)
	tx.Entries[0].Amount, tx.Entries[0].UserID, tx.Entries[0].Ref = n("999999999"), "mallory", RefUser
	mustApply(t, s, tx)

	bal(t, s, "alice", "1000", "0", "1000") // refunded exactly 400, to alice, not 999999999 to mallory
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-1000" {
		t.Fatalf("chain balance %s, want -1000", c)
	}
	if got := count(t, s, `SELECT count(*) FROM accounts WHERE user_id = 'mallory'`); got != 0 {
		t.Fatal("a correction created an account from caller-supplied data")
	}
	if st, err := s.Apply(ctx, tx); err != nil || st != AlreadyApplied { // replay
		t.Fatalf("replay: %v %v", st, err)
	}
	bal(t, s, "alice", "1000", "0", "1000")
}

func TestCorrection_CannotMintOrPartiallyReverse(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "400")
	mustSettle(t, s, "w1")
	ids := entryIDs(t, s, "w1")

	// only one leg: the reversal doesn't balance
	if _, err := s.Apply(ctx, correction("c-half", ids[0], ids[0])); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("same entry twice: %v", err)
	}
	dep := entryIDs(t, s, "d1")
	if _, err := s.Apply(ctx, correction("c-leg", dep[0], ids[1])); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("legs of different transactions: %v", err)
	}
	// an entry that doesn't exist
	if _, err := s.Apply(ctx, correction("c-ghost", "33333333-3333-3333-3333-333333333333", ids[0])); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("missing entry: %v", err)
	}
	bal(t, s, "alice", "600", "0", "600") // none of the above changed anything

	// an entry can be reversed once
	mustApply(t, s, correction("c1", ids...))
	if _, err := s.Apply(ctx, correction("c2", ids...)); !errors.Is(err, ErrInvalidCorrection) {
		t.Fatalf("second reversal of the same entries: %v", err)
	}
	bal(t, s, "alice", "1000", "0", "1000")
}

func TestCorrection_KeyReuseWithDifferentEntriesIsRejected(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustApply(t, s, deposit("d2", "alice", "500"))
	mustApply(t, s, correction("c1", entryIDs(t, s, "d1")...))
	if _, err := s.Apply(ctx, correction("c1", entryIDs(t, s, "d2")...)); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("got %v", err)
	}
}

// invariant #9: a correction debit is checked against Available, so it can't undercut a Lock
func TestCorrection_CannotUndercutAnActiveLock(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "800")
	// reversing the 1000 deposit would take total to 0 while 800 is locked
	if _, err := s.Apply(ctx, correction("c1", entryIDs(t, s, "d1")...)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	bal(t, s, "alice", "1000", "800", "200")
	if err := s.Release(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, correction("c1", entryIDs(t, s, "d1")...)) // fine once the lock is gone
	bal(t, s, "alice", "0", "0", "0")
}

// ---- schema backstops ------------------------------------------------------

func TestSchemaRefusesAnAvailableBalanceBelowZero(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	mustApply(t, s, deposit("d1", "alice", "100"))
	for name, q := range map[string]string{
		"locked above total": `UPDATE accounts SET locked_balance = 101 WHERE user_id = 'alice'`,
		"negative locked":    `UPDATE accounts SET locked_balance = -1 WHERE user_id = 'alice'`,
		"negative total":     `UPDATE accounts SET total_balance = -1 WHERE user_id = 'alice'`,
		"lock on chain acct": `UPDATE accounts SET locked_balance = 1 WHERE kind = 'chain'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
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
