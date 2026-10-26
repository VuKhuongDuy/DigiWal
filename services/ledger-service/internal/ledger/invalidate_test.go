package ledger

import (
	"context"
	"sync"
	"testing"
)

// Every committed change to a user's balances tells the read cache, once, and nothing else does (docs/design/read-cache.md).
func TestBalanceChangeHookFiresAfterEachCommitAndOnlyThen(t *testing.T) {
	s := newStore(t)
	var mu sync.Mutex
	var calls [][]string
	s.OnBalanceChange(func(_ context.Context, ids []string) {
		mu.Lock()
		calls = append(calls, ids)
		mu.Unlock()
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(calls) }
	step := func(name string, want int) {
		t.Helper()
		if got := count(); got != want {
			t.Fatalf("after %s the hook has fired %d times, want %d", name, got, want)
		}
	}

	mustApply(t, s, deposit("d1", "alice", "1000"))
	step("a deposit", 1)
	mustLock(t, s, "w1", "alice", "400")
	step("a Lock", 2)
	mustSettle(t, s, "w1")
	step("a Settle", 3)
	mustLock(t, s, "w2", "alice", "100")
	if err := s.Release(context.Background(), "w2"); err != nil {
		t.Fatal(err)
	}
	step("a Lock and a Release", 5)
	for _, c := range calls {
		if len(c) != 1 || c[0] != "alice" {
			t.Fatalf("hook arguments = %v, want [alice]", c)
		}
	}

	// Nothing committed, nothing announced: an insufficient-funds Lock rolls back; replays commit nothing.
	if _, err := s.Lock(context.Background(), lockReq("w3", "alice", "999999")); err == nil {
		t.Fatal("an overdraw Lock succeeded")
	}
	mustSettleAgain := func() {
		if _, err := s.Settle(context.Background(), "w1"); err != nil {
			t.Fatal(err)
		}
	}
	mustSettleAgain()
	if st, err := s.Apply(context.Background(), deposit("d1", "alice", "1000")); err != nil || st != AlreadyApplied {
		t.Fatalf("replayed deposit = %v %v", st, err)
	}
	step("a rolled-back Lock and two replays", 5)
}

// A correction names entries, not users; the hook must still get the owner of the corrected account.
func TestBalanceChangeHookReportsTheUserOfACorrection(t *testing.T) {
	s := newStore(t)
	mustApply(t, s, deposit("d1", "bob", "50"))
	ids := entryIDs(t, s, "d1")
	var got []string
	s.OnBalanceChange(func(_ context.Context, u []string) { got = append(got, u...) })
	mustApply(t, s, correction("c1", ids...))
	seen := map[string]bool{}
	for _, u := range got {
		seen[u] = true
	}
	if !seen["bob"] {
		t.Fatalf("hook users after a correction = %v, want bob", got)
	}
}
