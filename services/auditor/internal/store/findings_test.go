package store

import (
	"context"
	"os"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
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

func TestMetricsFromTheDatabase(t *testing.T) {
	s := connect(t)
	ctx := context.Background()
	finish := func(job string, start time.Time, dur time.Duration, res job.Result) {
		id, _ := s.StartRun(ctx, job, start)
		if err := s.FinishRun(ctx, id, start.Add(dur), res, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	finish("ledger_integrity", now.Add(-20*time.Minute), 2*time.Second, job.OK)
	finish("ledger_integrity", now.Add(-10*time.Minute), 3*time.Second, job.Fail)
	finish("reconciliation", now.Add(-30*time.Minute), time.Second, job.Warn)
	finish("reconciliation", now.Add(-5*time.Minute), time.Second, job.Error) // an error is the last run but not a success
	finish("never_ok", now.Add(-5*time.Minute), time.Second, job.Error)
	s.StartRun(ctx, "ledger_integrity", now) // a run that never finished (a crash) is not "the last run"

	m, err := s.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]JobStatus{}
	for _, j := range m.Jobs {
		by[j.Job] = j
	}
	li := by["ledger_integrity"]
	if li.LastResult != "fail" || li.LastDuration != 3*time.Second || !li.LastRunAt.Equal(now.Add(-10*time.Minute+3*time.Second)) || li.LastSuccess == nil || !li.LastSuccess.Equal(now.Add(-20*time.Minute+2*time.Second)) {
		t.Fatalf("ledger_integrity: %+v", li)
	}
	rc := by["reconciliation"]
	if rc.LastResult != "error" || rc.LastSuccess == nil || !rc.LastSuccess.Equal(now.Add(-30*time.Minute+time.Second)) {
		t.Fatalf("an error run must be the last result but never the last success: %+v", rc)
	}
	if by["never_ok"].LastSuccess != nil {
		t.Fatalf("a job that only ever errored has no last success: %+v", by["never_ok"])
	}

	// findings: two open (one critical, one warn), one resolved; the oldest-open age is per severity
	for run, v := range map[int64]finding.Violation{1: viol("W1", finding.Critical)} {
		_ = run
		v.Immediate = true
		if err := s.Reconcile(ctx, 1, "withdrawal", "a", []string{"W1"}, []finding.Violation{v}, now.Add(-90*time.Second), 0); err != nil {
			t.Fatal(err)
		}
	}
	w := viol("D6", finding.Warn)
	w.Immediate = true
	if err := s.Reconcile(ctx, 1, "deposit", "b", []string{"D6"}, []finding.Violation{w}, now, 0); err != nil {
		t.Fatal(err)
	}
	c := viol("W1", finding.Critical)
	c.Immediate = true
	s.Reconcile(ctx, 1, "withdrawal", "c", []string{"W1"}, []finding.Violation{c}, now, 0)
	s.Reconcile(ctx, 2, "withdrawal", "c", []string{"W1"}, nil, now, 0) // resolved
	m, _ = s.Metrics(ctx)
	open := map[string]int64{}
	for _, o := range m.Open {
		open[o.Flow+"/"+o.Rule+"/"+o.Severity] = o.Count
	}
	if len(open) != 2 || open["withdrawal/W1/critical"] != 1 || open["deposit/D6/warn"] != 1 {
		t.Fatalf("open: %v", open)
	}
	if m.OldestOpenAge["critical"] < 85 || m.OldestOpenAge["warn"] < 0 || m.OldestOpenAge["info"] != 0 || len(m.OldestOpenAge) != 3 {
		t.Fatalf("oldest: %v", m.OldestOpenAge)
	}
	var detected int64
	for _, d := range m.Detected {
		detected += d.Count
	}
	if detected != 3 { // a resolved row is still a finding that was recorded: the counter only grows
		t.Fatalf("detected = %d, want 3", detected)
	}

	// reconciliation figures: the latest per asset, in whole units; one without decimals cannot be shown
	for i, resid := range []string{"5", "-1500000"} {
		if err := s.AddSnapshot(ctx, Snapshot{RunID: int64(i), AssetID: "a1", Symbol: "TKN", Block: 10, OnChain: "500000000", LedgerCustody: "500000000", Explained: "0", Residual: resid, Tolerance: "0",
			Components: map[string]string{"decimals": "6"}, At: now.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddSnapshot(ctx, Snapshot{RunID: 9, AssetID: "old", Symbol: "OLD", Block: 1, OnChain: "1", LedgerCustody: "1", Explained: "0", Residual: "0", Tolerance: "0", Components: map[string]string{}, At: now})
	m, _ = s.Metrics(ctx)
	if len(m.Recon) != 1 || m.Recon[0].Asset != "TKN" || m.Recon[0].Residual != -1.5 || m.Recon[0].OnChain != 500 {
		t.Fatalf("recon: %+v", m.Recon)
	}
}

// Grafana's login (roles/grafana.sql) reads the four tables the Engineering dashboard shows and nothing else, and writes nothing.
func TestGrafanaRoleReadsOnlyTheAuditTables(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.New(t)
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body, err := os.ReadFile("../../roles/grafana.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, string(body)); err != nil { // idempotent
		t.Fatalf("second run: %v", err)
	}

	g, err := pgxpool.New(ctx, testdb.WithUser(t, dsn, testdb.DBName(t, dsn), "grafana_ro", "grafana_ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, tbl := range []string{"suspicious_transactions", "treasury_movements", "job_runs", "recon_snapshots"} {
		if _, err := g.Exec(ctx, "SELECT * FROM "+tbl+" LIMIT 0"); err != nil {
			t.Errorf("grafana_ro cannot read %s: %v", tbl, err)
		}
		for _, w := range []string{"DELETE FROM " + tbl, "TRUNCATE " + tbl} {
			if _, err := g.Exec(ctx, w); err == nil {
				t.Errorf("grafana_ro ran %q", w)
			}
		}
	}
	for _, tbl := range []string{"pending_findings", "audit_cursor", "schema_migrations"} {
		if _, err := g.Exec(ctx, "SELECT * FROM "+tbl+" LIMIT 0"); err == nil {
			t.Errorf("grafana_ro can read %s, which the dashboard does not need", tbl)
		}
	}
	if _, err := g.Exec(ctx, "INSERT INTO job_runs (job, started_at) VALUES ('x', now())"); err == nil {
		t.Error("grafana_ro inserted a row")
	}
	if _, err := g.Exec(ctx, "CREATE TABLE grafana_scratch (a int)"); err == nil {
		t.Error("grafana_ro created a table")
	}
}
