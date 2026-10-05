package ledger

import (
	"context"
	"testing"
	"time"
)

// backdate moves a table's created_at, bypassing the immutability trigger the way a superuser edit would, so a
// test can place activity on chosen UTC days.
func backdate(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func statsOf(t *testing.T, s *Store) Stats {
	t.Helper()
	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func asset(st Stats, id string) AssetStats {
	for _, a := range st.Assets {
		if a.ID == id {
			return a
		}
	}
	return AssetStats{}
}

func TestStatsDepositsAreMonotonicAndLiabilitiesAreNot(t *testing.T) {
	s := newStore(t)
	// nothing yet: every registered asset is present at zero (a series that starts at 0, not one that appears)
	st := statsOf(t, s)
	if len(st.Assets) != 2 || asset(st, eth).DepositsCredited != 0 || asset(st, eth).DepositAmount.Sign() != 0 {
		t.Fatalf("empty: %+v", st.Assets)
	}

	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustApply(t, s, deposit("d2", "bob", "500"))
	mustApply(t, s, Transaction{IdempotencyKey: "d3", Source: SourceDeposit,
		Deposit: &DepositRef{ChainID: 1, TxHash: "0x00000000000000000000000000000000000000000000000000000000000000d2", LogIndex: 3},
		Entries: []Entry{user("alice", tok, "70"), chain(tok, "-70")}})
	a := asset(statsOf(t, s), eth)
	if a.DepositsCredited != 2 || a.DepositAmount.String() != "1500" || a.UserLiabilities.String() != "1500" {
		t.Fatalf("eth after two deposits: %+v", a)
	}
	if tk := asset(statsOf(t, s), tok); tk.DepositsCredited != 1 || tk.DepositAmount.String() != "70" {
		t.Fatalf("token: %+v", tk)
	}

	// a withdrawal Settles: liabilities fall, the deposit counters (history) do not
	mustLock(t, s, "w1", "alice", "400")
	mustSettle(t, s, "w1")
	a = asset(statsOf(t, s), eth)
	if a.DepositsCredited != 2 || a.DepositAmount.String() != "1500" || a.UserLiabilities.String() != "1100" {
		t.Fatalf("after a withdrawal: %+v", a)
	}
}

func TestStatsActiveUserDays(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "alice", "1000")) // alice: Entry today
	mustApply(t, s, deposit("d2", "bob", "1000"))   // bob: Entry today
	mustLock(t, s, "w1", "alice", "10")             // alice again today: still one (day, alice)
	if got := statsOf(t, s).ActiveUserDays; got != 2 {
		t.Fatalf("two users, one day: %d", got)
	}

	// alice is also active in a second asset the same day: still once
	mustApply(t, s, Transaction{IdempotencyKey: "d3", Source: SourceDeposit,
		Deposit: &DepositRef{ChainID: 1, TxHash: "0x00000000000000000000000000000000000000000000000000000000000000d3", LogIndex: 0},
		Entries: []Entry{user("alice", tok, "5"), chain(tok, "-5")}})
	if got := statsOf(t, s).ActiveUserDays; got != 2 {
		t.Fatalf("a user active in two assets counts once per day: %d", got)
	}

	// move bob's Entry to yesterday and give carol a day that only has a Lock (she is active via the Lock alone)
	yesterday := time.Now().UTC().Add(-24 * time.Hour)
	backdate(t, s, `UPDATE entries SET created_at = $1 WHERE account_id = (SELECT id FROM accounts WHERE user_id = 'bob')`, yesterday)
	mustApply(t, s, deposit("d4", "carol", "100"))
	mustLock(t, s, "w2", "carol", "1")
	backdate(t, s, `UPDATE entries SET created_at = $1 WHERE account_id = (SELECT id FROM accounts WHERE user_id = 'carol')`, yesterday.Add(-24*time.Hour))
	// today: alice, (carol's Lock is today) ; yesterday: bob ; two days ago: carol's Entry
	if got := statsOf(t, s).ActiveUserDays; got != 4 {
		t.Fatalf("(today, alice) (today, carol via the Lock) (yesterday, bob) (2 days ago, carol): %d", got)
	}
}

func TestStatsFailedFlaggedAndLocks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	st := statsOf(t, s)
	if st.FailedEvents != 0 || st.ActiveLocks != 0 || st.OldestLockAge != 0 {
		t.Fatalf("empty: %+v", st)
	}
	for _, status := range []string{"pending", "approved", "rejected"} {
		if v, ok := st.Flagged[status]; !ok || v != 0 {
			t.Fatalf("flagged %s must be present at 0: %v", status, st.Flagged)
		}
	}

	if err := s.RecordFailedEvent(ctx, "deposits.confirmed", "k", []byte("{}"), "bad"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertFlaggedDeposit(ctx, FlaggedDeposit{ChainID: 1, TxHash: "0xaa", LogIndex: 0, AssetID: eth, FromAddress: "0x1", ToAddress: "0x2", Amount: n("5"), BlockNumber: 1}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, deposit("d1", "alice", "1000"))
	mustLock(t, s, "w1", "alice", "10")
	mustLock(t, s, "w2", "alice", "20")
	backdate(t, s, `UPDATE locks SET created_at = now() - interval '90 seconds' WHERE idempotency_key = 'w1'`)
	st = statsOf(t, s)
	if st.FailedEvents != 1 || st.Flagged["pending"] != 1 || st.ActiveLocks != 2 {
		t.Fatalf("%+v", st)
	}
	if st.OldestLockAge < 89*time.Second || st.OldestLockAge > 120*time.Second {
		t.Fatalf("oldest lock age %s", st.OldestLockAge)
	}
	// a Settled or Released Lock is no longer active
	mustSettle(t, s, "w1")
	if err := s.Release(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if st = statsOf(t, s); st.ActiveLocks != 0 || st.OldestLockAge != 0 {
		t.Fatalf("%+v", st)
	}
}
