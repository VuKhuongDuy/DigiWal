package store

import (
	"context"
	"errors"
	"math/big"
	"os"
	"regexp"
	"sync"
	"testing"

	"digiwal/services/withdrawal-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func req(user, key string) NewRequest {
	return NewRequest{UserID: user, IdempotencyKey: key, AssetID: "11111111-1111-1111-1111-111111111111",
		Amount:    new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), // beyond int64: NUMERIC round-trip
		ToAddress: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestCreateThenDuplicateReadsBack(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	w, created, err := s.Create(ctx, req("alice", "k1"))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if w.Status != StatusRequested || !uuidRe.MatchString(w.ID) || w.Amount.Cmp(req("", "").Amount) != 0 || w.TxHash != nil {
		t.Fatalf("new row = %+v", w)
	}
	if w.Key() != "withdrawal:"+w.ID {
		t.Fatalf("Withdrawal Key = %q", w.Key())
	}
	again, created, err := s.Create(ctx, req("alice", "k1"))
	if err != nil || created || again.ID != w.ID || again.Status != StatusRequested {
		t.Fatalf("duplicate: created=%v id=%s (want %s) err=%v", created, again.ID, w.ID, err)
	}
}

// invariant #11: a raced duplicate creates exactly one row, and exactly one caller "created" it.
func TestConcurrentDuplicatesCreateOneRow(t *testing.T) {
	s := newStore(t)
	const n = 32
	var wg sync.WaitGroup
	ids := make([]string, n)
	createdCount := 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, created, err := s.Create(context.Background(), req("alice", "race"))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			ids[i] = w.ID
			if created {
				createdCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatalf("%d callers created a row, want exactly 1", createdCount)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("callers saw different rows: %v", ids)
		}
	}
	var rows int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM withdrawal_requests`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d (%v)", rows, err)
	}
}

// invariant #20: the key is scoped per user — same key, two users, two independent requests,
// and neither can read the other's.
func TestKeyIsScopedPerUser(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, createdA, err := s.Create(ctx, req("alice", "shared"))
	if err != nil {
		t.Fatal(err)
	}
	b, createdB, err := s.Create(ctx, req("bob", "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if !createdA || !createdB || a.ID == b.ID || a.Key() == b.Key() {
		t.Fatalf("same key for two users must be two requests: a=%+v b=%+v", a, b)
	}
	if got, err := s.Get(ctx, "bob", "shared"); err != nil || got.ID != b.ID {
		t.Fatalf("bob reads %s (%v), want his own %s", got.ID, err, b.ID)
	}
	if _, err := s.Get(ctx, "carol", "shared"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("carol reading another user's key: %v, want ErrNotFound", err)
	}
}

func TestSchemaRejectsBadRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	zero := req("alice", "zero")
	zero.Amount = big.NewInt(0)
	if _, _, err := s.Create(ctx, zero); err == nil {
		t.Error("zero amount accepted")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET status = 'bogus'`); err != nil {
		t.Fatal(err) // no rows yet: fine
	}
	if _, _, err := s.Create(ctx, req("alice", "ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET status = 'bogus'`); err == nil {
		t.Error("unknown status accepted")
	}
}

// invariant #11: transitions are guarded, so a second writer from the same status loses.
func TestTransitionIsGuarded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	w, _, _ := s.Create(ctx, req("alice", "t1"))
	reason := "below_minimum_withdrawal_amount"
	got, err := s.Transition(ctx, w.ID, StatusRequested, StatusRejected, &reason)
	if err != nil || got.Status != StatusRejected || got.RejectReason == nil || *got.RejectReason != reason || !got.UpdatedAt.After(w.UpdatedAt) {
		t.Fatalf("first transition: %+v %v", got, err)
	}
	if _, err := s.Transition(ctx, w.ID, StatusRequested, StatusFailed, nil); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("second writer: %v, want ErrStateChanged", err)
	}
	if again, _ := s.Get(ctx, "alice", "t1"); again.Status != StatusRejected {
		t.Fatalf("row overwritten: %s", again.Status)
	}
}
