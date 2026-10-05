package store

import (
	"context"
	"os"
	"testing"
	"time"

	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

func TestRunHistorySurvivesReconnect(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.New(t)
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	if _, ok, _ := s.LastRun(ctx, "ledger_integrity"); ok {
		t.Fatal("a job that never ran has no last run")
	}

	okID, _ := s.StartRun(ctx, "ledger_integrity", t0)
	if err := s.FinishRun(ctx, okID, t0.Add(time.Second), job.OK, 0, ""); err != nil {
		t.Fatal(err)
	}
	errID, _ := s.StartRun(ctx, "ledger_integrity", t0.Add(time.Minute))
	if err := s.FinishRun(ctx, errID, t0.Add(time.Minute+time.Second), job.Error, 0, "rpc down"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, errID, t0, job.OK, 0, ""); err == nil {
		t.Error("a finished run must not be finished again")
	}
	crashed, _ := s.StartRun(ctx, "ledger_integrity", t0.Add(2*time.Minute)) // never finished: a crash
	s.Close()

	s, err = Connect(ctx, dsn) // a restart; migrations are idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	last, ok, err := s.LastRun(ctx, "ledger_integrity")
	if err != nil || !ok || last.ID != crashed || last.Result != "" {
		t.Fatalf("last run should be the unfinished one: %+v ok=%v err=%v", last, ok, err)
	}
	succ, ok, err := s.LastSuccess(ctx, "ledger_integrity")
	if err != nil || !ok || succ.ID != okID {
		t.Fatalf("last success must skip error and unfinished runs: %+v ok=%v err=%v", succ, ok, err)
	}
	prev, _, _ := s.one(ctx, `WHERE id = $1`, errID)
	if prev.Result != job.Error || prev.Error != "rpc down" {
		t.Errorf("error run not stored: %+v", prev)
	}
}

func TestResultAndFinishedAtAgree(t *testing.T) {
	ctx := context.Background()
	s, err := Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.pool.Exec(ctx, `INSERT INTO job_runs (job, started_at, result) VALUES ('x', now(), 'ok')`)
	if err == nil {
		t.Fatal("a result without finished_at must be rejected")
	}
}
