// Package testdb hands tests a fresh database on a real Postgres: TEST_POSTGRES_DSN
// if set, otherwise an ephemeral testcontainers-go container, otherwise the test
// is skipped. Call Terminate from TestMain.
package testdb

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	adminOnce sync.Once
	adminDSN  string // DSN of a real Postgres server we can CREATE DATABASE on
	adminErr  error
	dbCounter int
	dbMu      sync.Mutex
)

var container testcontainers.Container

// Terminate stops the shared container, if this process started one.
func Terminate() {
	if container != nil {
		_ = container.Terminate(context.Background())
	}
}

// New returns the DSN of a fresh, empty database on a real
// Postgres. It uses TEST_POSTGRES_DSN (a server we may CREATE DATABASE
// on) if set, otherwise starts an ephemeral container via
// testcontainers-go, otherwise skips.
func New(t *testing.T) string {
	t.Helper()
	adminOnce.Do(func() {
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
	dbMu.Lock()
	dbCounter++
	name := fmt.Sprintf("walletsvc_test_%d_%d", os.Getpid(), dbCounter)
	dbMu.Unlock()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pgxpool.New(ctx, adminDSN)
		if err == nil {
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
