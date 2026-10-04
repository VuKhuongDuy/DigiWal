// Package testdb hands tests a fresh database on a real Postgres: TEST_POSTGRES_DSN if set (a server we
// may CREATE DATABASE and CREATE ROLE on), otherwise an ephemeral testcontainers-go container, otherwise
// the test is skipped. Call Terminate from TestMain.
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
	once      sync.Once
	adminDSN  string
	adminErr  error
	container testcontainers.Container
	n         int
	mu        sync.Mutex
)

// Terminate stops the shared container, if this process started one.
func Terminate() {
	if container != nil {
		_ = container.Terminate(context.Background())
	}
}

// Admin returns the superuser DSN of the test server (skipping the test if there is none).
func Admin(t *testing.T) string {
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
	return adminDSN
}

// New returns the DSN of a fresh, empty database (as the superuser).
func New(t *testing.T) string {
	t.Helper()
	dsn := Admin(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mu.Lock()
	n++
	name := fmt.Sprintf("auditor_test_%d_%d", os.Getpid(), n)
	mu.Unlock()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a, err := pgxpool.New(ctx, dsn); err == nil {
			_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close()
		}
	})
	return WithUser(t, dsn, name, "", "")
}

// WithUser rewrites a DSN to another database and, if user is set, another user.
func WithUser(t *testing.T, dsn, database, user, password string) string {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if user == "" {
		user, password = cfg.ConnConfig.User, cfg.ConnConfig.Password
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", user, password, cfg.ConnConfig.Host, cfg.ConnConfig.Port, database)
}
