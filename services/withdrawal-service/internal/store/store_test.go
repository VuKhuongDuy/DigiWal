package store

import (
	"context"
	"errors"
	"math/big"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

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
	if _, err := s.Transition(ctx, w.ID, StatusRequested, StatusValidated, nil); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("second writer: %v, want ErrStateChanged", err)
	}
	if again, _ := s.Get(ctx, "alice", "t1"); again.Status != StatusRejected {
		t.Fatalf("row overwritten: %s", again.Status)
	}
}

func TestListOlderThanAndStampBroadcastHead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	w, _, err := s.Create(ctx, req("alice", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	future := w.UpdatedAt.Add(time.Minute)
	if rows, err := s.ListOlderThan(ctx, StatusRequested, w.UpdatedAt.Add(-time.Minute), 10); err != nil || len(rows) != 0 {
		t.Fatalf("a fresh row listed as old: %v %v", rows, err)
	}
	if rows, err := s.ListOlderThan(ctx, StatusValidated, future, 10); err != nil || len(rows) != 0 {
		t.Fatalf("wrong status listed: %v %v", rows, err)
	}
	rows, err := s.ListOlderThan(ctx, StatusRequested, future, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != w.ID {
		t.Fatalf("old row not listed: %v %v", rows, err)
	}

	// The stamp only lands on a `broadcast` row, and only once.
	if err := s.StampBroadcastHead(ctx, w.ID, 7); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByID(ctx, w.ID); got.BroadcastHeadSeen != nil {
		t.Fatal("stamped a row that is not broadcast")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET status = 'broadcast' WHERE withdrawal_id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	for _, head := range []int64{7, 99} {
		if err := s.StampBroadcastHead(ctx, w.ID, head); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.GetByID(ctx, w.ID); got.BroadcastHeadSeen == nil || *got.BroadcastHeadSeen != 7 {
		t.Fatalf("stamp = %v, want the first head (7)", got.BroadcastHeadSeen)
	}
}

func TestAddReplacementIsGuardedIdempotentAndResetsSentHead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	w, _, err := s.Create(ctx, req("alice", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	// Only a signed/broadcast request takes a Fee Bump.
	if err := s.AddReplacement(ctx, w.ID, 1, "0xaa", "0x01"); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("on a requested row: %v", err)
	}
	if err := s.AddReplacement(ctx, "00000000-0000-0000-0000-000000000000", 1, "0xaa", "0x01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET status = 'broadcast', sent_head = 100 WHERE withdrawal_id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReplacement(ctx, w.ID, 1, "0xaa", "0x01"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetByID(ctx, w.ID)
	if got.SentHead != nil {
		t.Fatal("a new version must be marked not sent yet")
	}
	if err := s.MarkSent(ctx, w.ID, 130); err != nil {
		t.Fatal(err)
	}
	// A replay of the same seq changes nothing — in particular it does not unmark the version as sent.
	if err := s.AddReplacement(ctx, w.ID, 1, "0xaa", "0x01"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetByID(ctx, w.ID)
	if got.SentHead == nil || *got.SentHead != 130 {
		t.Fatalf("sent_head = %v after a replay, want 130", got.SentHead)
	}
	if err := s.AddReplacement(ctx, w.ID, 2, "0xbb", "0x02"); err != nil {
		t.Fatal(err)
	}
	txs, err := s.ListTxs(ctx, w.ID)
	if err != nil || len(txs) != 2 || txs[0].Seq != 1 || txs[1].Hash != "0xbb" || txs[1].Raw != "0x02" {
		t.Fatalf("%+v %v", txs, err)
	}
	// MarkSent only touches a broadcast row.
	if _, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET status = 'confirmed', sent_head = NULL WHERE withdrawal_id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	_ = s.MarkSent(ctx, w.ID, 999)
	if got, _ := s.GetByID(ctx, w.ID); got.SentHead != nil {
		t.Fatal("MarkSent stamped a confirmed row")
	}
}

// invariant #24: every `→ failed` writes its event in the same transaction, once.
func TestMarkFailedWritesTheEventOnceAndTransitionRefusesFailed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	w, _, _ := s.Create(ctx, req("alice", "f1"))
	if _, err := s.Transition(ctx, w.ID, StatusRequested, StatusFailed, nil); err == nil {
		t.Fatal("Transition to failed must be refused: it would skip the event")
	}
	ev := func(u Withdrawal) (string, []byte, error) {
		return "withdrawals.failed", []byte(`{"id":"` + u.ID + `"}`), nil
	}
	got, err := s.MarkFailed(ctx, w.ID, StatusRequested, "stale_request", "", nil, ev)
	if err != nil || got.Status != StatusFailed || *got.RejectReason != "stale_request" {
		t.Fatalf("%+v %v", got, err)
	}
	// A second writer loses, and no second event appears.
	if _, err := s.MarkFailed(ctx, w.ID, StatusRequested, "stale_request", "", nil, ev); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("second writer: %v, want ErrStateChanged", err)
	}
	evs, _ := s.UnpublishedEvents(ctx, 10)
	if len(evs) != 1 || evs[0].Topic != "withdrawals.failed" || evs[0].WithdrawalID != w.ID {
		t.Fatalf("outbox = %+v", evs)
	}
	// An event that cannot be built fails the whole transition: nothing half-done.
	w2, _, _ := s.Create(ctx, req("alice", "f2"))
	if _, err := s.MarkFailed(ctx, w2.ID, StatusRequested, "x", "", nil, func(Withdrawal) (string, []byte, error) { return "", nil, errors.New("boom") }); err == nil {
		t.Fatal("expected the event error")
	}
	if again, _ := s.GetByID(ctx, w2.ID); again.Status != StatusRequested {
		t.Fatalf("a failed event write left the row %s", again.Status)
	}
}

// invariant #25: blocked wins over pending/active, in the same transaction as `→ failed`; only
// delete + re-add (a new row, a new cooldown) lifts it; other users' entries are untouched.
func TestMarkFailedCanBlockTheWhitelistEntry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const addr = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	alice, _, _ := s.AddWhitelistEntry(ctx, "alice", addr)
	_, _, _ = s.AddWhitelistEntry(ctx, "bob", addr)
	if _, err := s.pool.Exec(ctx, `UPDATE whitelist_entries SET added_at = now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if st, ok, _ := s.WhitelistStatus(ctx, "alice", addr, 24*time.Hour); !ok || st != WhitelistActive {
		t.Fatalf("setup: %s", st)
	}
	w, _, _ := s.Create(ctx, req("alice", "f1"))
	if _, err := s.MarkFailed(ctx, w.ID, StatusRequested, "tx_reverted", addr, nil, func(Withdrawal) (string, []byte, error) { return "t", []byte("{}"), nil }); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.WhitelistStatus(ctx, "alice", addr, 24*time.Hour); st != WhitelistBlocked {
		t.Fatalf("alice's entry is %s, want blocked", st)
	}
	if st, _, _ := s.WhitelistStatus(ctx, "bob", addr, 24*time.Hour); st != WhitelistActive {
		t.Fatalf("bob's entry is %s: blocking must not touch other users", st)
	}
	list, _ := s.ListWhitelist(ctx, "alice")
	if len(list) != 1 || list[0].Status(time.Now(), 24*time.Hour) != WhitelistBlocked {
		t.Fatalf("list = %+v", list)
	}
	// Re-adding without deleting is a no-op (stays blocked, same cooldown clock) ...
	again, created, _ := s.AddWhitelistEntry(ctx, "alice", addr)
	if created || again.ID != alice.ID || again.Status(time.Now(), 24*time.Hour) != WhitelistBlocked {
		t.Fatalf("re-add = %+v created=%v", again, created)
	}
	// ... delete + re-add is a brand-new pending entry.
	_ = s.RemoveWhitelistEntry(ctx, "alice", alice.ID)
	fresh, created, _ := s.AddWhitelistEntry(ctx, "alice", addr)
	if !created || fresh.ID == alice.ID || fresh.Status(time.Now(), 24*time.Hour) != WhitelistPending {
		t.Fatalf("after delete + re-add: %+v created=%v", fresh, created)
	}
	if st, _, _ := s.WhitelistStatus(ctx, "alice", addr, 24*time.Hour); st != WhitelistPending {
		t.Fatalf("status %s, want pending", st)
	}
}
