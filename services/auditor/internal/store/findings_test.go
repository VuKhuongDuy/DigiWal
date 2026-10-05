package store

import (
	"context"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/testdb"
)

var (
	rules = []string{"W1", "W5"}
	t0    = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
)

func viol(rule, sev string) finding.Violation {
	return finding.Violation{Rule: rule, Severity: sev, Detail: map[string]any{"problem": "x"}}
}

type row struct {
	status, severity string
	first, last      int64
}

func rowOf(t *testing.T, s *Store, rule string) (row, bool) {
	t.Helper()
	var r row
	err := s.pool.QueryRow(context.Background(), `SELECT status, severity, first_run_id, last_run_id FROM suspicious_transactions
		WHERE flow='withdrawal' AND reference='r1' AND rule=$1`, rule).Scan(&r.status, &r.severity, &r.first, &r.last)
	return r, err == nil
}

func pending(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM pending_findings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func connect(t *testing.T) *Store {
	t.Helper()
	s, err := Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func rec(t *testing.T, s *Store, run int64, at time.Time, vs ...finding.Violation) {
	t.Helper()
	if err := s.Reconcile(context.Background(), run, "withdrawal", "r1", rules, vs, at, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// Invariant 4: seen once = remembered; seen in the next run = recorded.
func TestTwoPass(t *testing.T) {
	s := connect(t)
	rec(t, s, 1, t0, viol("W1", finding.Critical))
	if _, ok := rowOf(t, s, "W1"); ok || pending(t, s) != 1 {
		t.Fatalf("first sighting must only be pending (row=%v pending=%d)", ok, pending(t, s))
	}
	rec(t, s, 2, t0.Add(5*time.Minute), viol("W1", finding.Critical))
	r, ok := rowOf(t, s, "W1")
	if !ok || r.status != "open" || r.first != 1 || r.last != 2 || pending(t, s) != 0 {
		t.Fatalf("second sighting must record it, first_run kept: %+v ok=%v pending=%d", r, ok, pending(t, s))
	}
}

func TestMismatchThatGoesAwayLeavesNothing(t *testing.T) {
	s := connect(t)
	rec(t, s, 1, t0, viol("W1", finding.Critical))
	rec(t, s, 2, t0.Add(5*time.Minute)) // lag caught up
	rec(t, s, 3, t0.Add(10*time.Minute), viol("W1", finding.Critical))
	if _, ok := rowOf(t, s, "W1"); ok {
		t.Fatal("a mismatch seen in runs 1 and 3 but not 2 is not 'two consecutive runs'")
	}
}

func TestImmediateIsRecordedAtOnce(t *testing.T) {
	s := connect(t)
	v := viol("W1", finding.Critical)
	v.Immediate = true
	rec(t, s, 1, t0, v)
	if r, ok := rowOf(t, s, "W1"); !ok || r.status != "open" || pending(t, s) != 0 {
		t.Fatalf("immediate: %+v %v", r, ok)
	}
}

// Invariant 5: never deleted; resolved when it goes away, reopened when it returns, one row per rule.
func TestResolveReopenAndIdempotency(t *testing.T) {
	s := connect(t)
	rec(t, s, 1, t0, viol("W1", finding.Critical))
	rec(t, s, 2, t0.Add(time.Minute), viol("W1", finding.Critical))
	rec(t, s, 3, t0.Add(2*time.Minute), viol("W1", finding.Critical)) // again: still one row
	var n int
	s.pool.QueryRow(context.Background(), `SELECT count(*) FROM suspicious_transactions`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	rec(t, s, 4, t0.Add(3*time.Minute)) // fixed
	if r, _ := rowOf(t, s, "W1"); r.status != "resolved" {
		t.Fatalf("should be resolved: %+v", r)
	}
	rec(t, s, 5, t0.Add(4*time.Minute), viol("W1", finding.Critical)) // came back; row exists, so no second pass needed
	if r, _ := rowOf(t, s, "W1"); r.status != "open" {
		t.Fatalf("should reopen: %+v", r)
	}
}

func TestAcknowledgedStaysAcknowledged(t *testing.T) {
	s := connect(t)
	rec(t, s, 1, t0, viol("W1", finding.Critical))
	rec(t, s, 2, t0.Add(time.Minute), viol("W1", finding.Critical))
	if _, err := s.pool.Exec(context.Background(), `UPDATE suspicious_transactions SET status='acknowledged', note='known' WHERE rule='W1'`); err != nil {
		t.Fatal(err)
	}
	rec(t, s, 3, t0.Add(2*time.Minute), viol("W1", finding.Critical))
	if r, _ := rowOf(t, s, "W1"); r.status != "acknowledged" {
		t.Fatalf("a human's acknowledgement must survive: %+v", r)
	}
	refs, _ := s.OpenRefs(context.Background(), "withdrawal")
	if len(refs) != 1 {
		t.Fatalf("acknowledged references are still re-checked: %v", refs)
	}
}

// "warn, then critical" (W5/D6), and a critical is never downgraded.
func TestEscalation(t *testing.T) {
	s := connect(t)
	w := viol("W5", finding.Warn)
	w.EscalateTo = finding.Critical
	rec(t, s, 1, t0, w)
	rec(t, s, 2, t0.Add(5*time.Minute), w)
	if r, _ := rowOf(t, s, "W5"); r.severity != "warn" {
		t.Fatalf("should be warn first: %+v", r)
	}
	rec(t, s, 3, t0.Add(5*time.Minute+16*time.Minute), w)
	if r, _ := rowOf(t, s, "W5"); r.severity != "critical" {
		t.Fatalf("should escalate after the window: %+v", r)
	}
	rec(t, s, 4, t0.Add(time.Hour), viol("W5", finding.Warn))
	if r, _ := rowOf(t, s, "W5"); r.severity != "critical" {
		t.Fatalf("must not downgrade: %+v", r)
	}
	warn, crit, _ := s.CountOpen(context.Background(), "withdrawal")
	if warn != 0 || crit != 1 {
		t.Fatalf("CountOpen = %d warn %d critical", warn, crit)
	}
}

func TestCursor(t *testing.T) {
	s := connect(t)
	ctx := context.Background()
	if c, err := s.Cursor(ctx, "j"); c != "" || err != nil {
		t.Fatalf("new cursor: %q %v", c, err)
	}
	s.SetCursor(ctx, "j", "a", t0)
	s.SetCursor(ctx, "j", "b", t0)
	if c, _ := s.Cursor(ctx, "j"); c != "b" {
		t.Fatalf("cursor = %q", c)
	}
}

// A critical that was fixed and later comes back as a warn is a warn: the old severity belongs to the old occurrence.
func TestReopenedFindingTakesItsNewSeverity(t *testing.T) {
	s := connect(t)
	rec(t, s, 1, t0, viol("W1", finding.Critical))
	rec(t, s, 2, t0.Add(time.Minute), viol("W1", finding.Critical))
	rec(t, s, 3, t0.Add(2*time.Minute)) // resolved
	rec(t, s, 4, t0.Add(3*time.Minute), viol("W1", finding.Warn))
	if r, _ := rowOf(t, s, "W1"); r.status != "open" || r.severity != "warn" {
		t.Fatalf("%+v", r)
	}
}
