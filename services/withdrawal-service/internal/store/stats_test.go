package store

import (
	"context"
	"testing"
	"time"
)

const (
	assetA = "11111111-1111-1111-1111-111111111111"
	assetB = "22222222-2222-2222-2222-222222222222"
)

var th = StuckThresholds{Request: 5 * time.Minute, Reservation: 5 * time.Minute, BroadcastBlocks: 128}

func exec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func insert(t *testing.T, s *Store, key, status, asset, amount string) string {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(), `INSERT INTO withdrawal_requests (user_id, idempotency_key, asset_id, amount, to_address, status)
		VALUES ('u', $1, $2, $3::numeric, '0x70997970C51812dc3A010C7d01b50e0d17dc79C8', $4) RETURNING withdrawal_id::text`, key, asset, amount, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func stats(t *testing.T, s *Store) Stats {
	t.Helper()
	st, err := s.Stats(context.Background(), th)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStatsEmptyHasEveryStatusAtZero(t *testing.T) {
	st := stats(t, newStore(t))
	if st.Created != 0 || len(st.ByStatus) != len(AllStatuses) || st.TimedCount != 0 || len(st.Failures) != 0 {
		t.Fatalf("%+v", st)
	}
	for _, status := range AllStatuses {
		if v, ok := st.ByStatus[status]; !ok || v != 0 {
			t.Errorf("status %s must be present at 0", status)
		}
	}
	for _, status := range nonTerminal {
		if v, ok := st.OldestAge[status]; !ok || v != 0 {
			t.Errorf("oldest age of %s must be present at 0", status)
		}
	}
}

func TestStatsCountsAmountsFailuresAndTimings(t *testing.T) {
	s := newStore(t)
	insert(t, s, "a", "requested", assetA, "10")
	id1 := insert(t, s, "b", "confirmed", assetA, "100")
	id2 := insert(t, s, "c", "confirmed", assetA, "50")
	insert(t, s, "d", "confirmed", assetB, "7")
	insert(t, s, "e", "rejected", assetA, "1")
	insert(t, s, "f", "failed", assetA, "1")
	exec(t, s, `UPDATE withdrawal_requests SET reject_reason = 'destination_not_whitelisted' WHERE idempotency_key = 'e'`)
	exec(t, s, `UPDATE withdrawal_requests SET reject_reason = 'tx_reverted' WHERE idempotency_key = 'f'`)
	insert(t, s, "g", "failed", assetA, "1")
	exec(t, s, `UPDATE withdrawal_requests SET reject_reason = 'tx_reverted' WHERE idempotency_key = 'g'`)

	// two confirmed requests with timings: ours 10s + 20s, the chain's 100s + 300s
	exec(t, s, `UPDATE withdrawal_requests SET created_at = now() - interval '1000 seconds', broadcast_at = now() - interval '990 seconds', confirmed_at = now() - interval '890 seconds' WHERE withdrawal_id = $1`, id1)
	exec(t, s, `UPDATE withdrawal_requests SET created_at = now() - interval '1000 seconds', broadcast_at = now() - interval '980 seconds', confirmed_at = now() - interval '680 seconds' WHERE withdrawal_id = $1`, id2)

	st := stats(t, s)
	if st.Created != 7 || st.ByStatus["confirmed"] != 3 || st.ByStatus["requested"] != 1 || st.ByStatus["failed"] != 2 || st.ByStatus["rejected"] != 1 {
		t.Fatalf("counts: created %d %v", st.Created, st.ByStatus)
	}
	if st.Confirmed[assetA].String() != "150" || st.Confirmed[assetB].String() != "7" {
		t.Fatalf("confirmed amounts per asset: %v", st.Confirmed)
	}
	got := map[string]int64{}
	for _, f := range st.Failures {
		got[f.Status+"/"+f.Reason] += f.Count
	}
	if got["failed/tx_reverted"] != 2 || got["rejected/destination_not_whitelisted"] != 1 || len(got) != 2 {
		t.Fatalf("failures: %v", got)
	}
	if st.TimedCount != 2 || st.ProcessingSecs < 29.9 || st.ProcessingSecs > 30.1 || st.ChainWaitSecs < 399.9 || st.ChainWaitSecs > 400.1 {
		t.Fatalf("timings: n=%d ours=%.1f chain=%.1f", st.TimedCount, st.ProcessingSecs, st.ChainWaitSecs)
	}
}

// The stuck counts and the Reaper's own query must agree at the boundary (monitoring-dashboard.md invariant 2).
func TestStuckAgreesWithTheReapersCutoffsAtTheBoundary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, status := range []string{"requested", "validated", "reserved", "signed"} {
		insert(t, s, status+"-old", status, assetA, "1")
		insert(t, s, status+"-fresh", status, assetA, "1")
		insert(t, s, status+"-edge", status, assetA, "1")
		exec(t, s, `UPDATE withdrawal_requests SET updated_at = now() - interval '301 seconds' WHERE idempotency_key = $1`, status+"-old")
		exec(t, s, `UPDATE withdrawal_requests SET updated_at = now() - interval '299 seconds' WHERE idempotency_key = $1`, status+"-edge")
		exec(t, s, `UPDATE withdrawal_requests SET updated_at = now() WHERE idempotency_key = $1`, status+"-fresh")
	}
	st := stats(t, s)
	for status, age := range map[string]time.Duration{"requested": th.Request, "validated": th.Reservation, "reserved": th.Reservation, "signed": th.Reservation} {
		reaper, err := s.ListOlderThan(ctx, status, time.Now().Add(-age), 100)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(reaper)) != st.Stuck[status] || st.Stuck[status] != 1 {
			t.Errorf("%s: metrics say %d stuck, the Reaper would reclaim %d, want 1", status, st.Stuck[status], len(reaper))
		}
	}
	if age := st.OldestAge["requested"]; age < 300 || age > 310 {
		t.Errorf("oldest requested age = %.1f", age)
	}
	if st.OldestAge["broadcast"] != 0 {
		t.Errorf("no broadcast requests: %v", st.OldestAge["broadcast"])
	}
}

func TestStuckBroadcastUsesBlocksSinceFirstSeen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for key, seen := range map[string]int64{"old": 100, "edge": 101, "fresh": 200} {
		insert(t, s, key, "broadcast", assetA, "1")
		exec(t, s, `UPDATE withdrawal_requests SET broadcast_head_seen = $2 WHERE idempotency_key = $1`, key, seen)
	}
	insert(t, s, "unseen", "broadcast", assetA, "1") // the Reaper has not stamped it yet: never stuck
	insert(t, s, "done", "confirmed", assetA, "1")
	exec(t, s, `UPDATE withdrawal_requests SET broadcast_head_seen = 1 WHERE idempotency_key = 'done'`)
	// head 229: old is 129 blocks ago (stuck), edge is 128 (stuck: >=), fresh 29
	if n, err := s.StuckBroadcast(ctx, 229, 128); err != nil || n != 2 {
		t.Fatalf("StuckBroadcast = %d %v, want 2", n, err)
	}
	if n, _ := s.StuckBroadcast(ctx, 228, 128); n != 1 {
		t.Fatalf("one block earlier only the oldest is stuck: %d", n)
	}
}

func TestStatsNonceGapsAndBlockedWhitelist(t *testing.T) {
	s := newStore(t)
	exec(t, s, `INSERT INTO nonce_gaps (chain_id, from_nonce, tx_hash, reported_at) VALUES (1, 5, '0xa', now()), (1, 9, '0xb', NULL)`)
	exec(t, s, `INSERT INTO whitelist_entries (user_id, address, blocked_at) VALUES ('u', '0x70997970C51812dc3A010C7d01b50e0d17dc79C8', now()), ('u', '0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC', NULL)`)
	st := stats(t, s)
	if st.NonceGaps != 2 || st.NonceGapsUnreported != 1 || st.WhitelistBlocked != 1 {
		t.Fatalf("%+v", st)
	}
}
