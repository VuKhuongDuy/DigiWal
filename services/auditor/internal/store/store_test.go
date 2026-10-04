package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"digiwal/services/auditor/internal/job"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	once      sync.Once
	adminDSN  string
	adminErr  error
	container testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if container != nil {
		_ = container.Terminate(context.Background())
	}
	os.Exit(code)
}

// newDSN returns a fresh database on a real Postgres: TEST_POSTGRES_DSN if set, else a container, else skip.
func newDSN(t *testing.T) string {
	t.Helper()
	once.Do(func() {
		if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
			adminDSN = dsn
			return
		}
		ctx := context.Background()
		c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
			tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
		if err != nil {
			adminErr = err
			return
		}
		container = c
		adminDSN, adminErr = c.ConnectionString(ctx, "sslmode=disable")
	})
	if adminErr != nil {
		t.Skipf("no Postgres available (set TEST_POSTGRES_DSN or run Docker): %v", adminErr)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("auditor_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := pgxpool.New(ctx, adminDSN); err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close()
		}
	})
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.ConnConfig.User, cfg.ConnConfig.Password, cfg.ConnConfig.Host, cfg.ConnConfig.Port, name)
}

func TestRunHistorySurvivesReconnect(t *testing.T) {
	ctx := context.Background()
	dsn := newDSN(t)
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
	s, err := Connect(ctx, newDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.pool.Exec(ctx, `INSERT INTO job_runs (job, started_at, result) VALUES ('x', now(), 'ok')`)
	if err == nil {
		t.Fatal("a result without finished_at must be rejected")
	}
}
