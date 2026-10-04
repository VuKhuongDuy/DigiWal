// Package store is the auditor's own Postgres, the only database it writes to
// (docs/design/reconciliation-audit.md invariant 1).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/migrations"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func Connect(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	if err := MigrateUp(dsn); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func MigrateUp(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

func (s *Store) StartRun(ctx context.Context, name string, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO job_runs (job, started_at) VALUES ($1, $2) RETURNING id`, name, at).Scan(&id)
	return id, err
}

func (s *Store) FinishRun(ctx context.Context, id int64, at time.Time, result job.Result, findings int, errMsg string) error {
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE job_runs SET finished_at = $2, result = $3, findings = $4, error = $5 WHERE id = $1 AND finished_at IS NULL`,
		id, at, string(result), findings, msg)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("job run %d is unknown or already finished", id)
	}
	return err
}

// Run is one row of job_runs. Result is empty while the run is in progress (or it crashed).
type Run struct {
	ID         int64
	Job        string
	StartedAt  time.Time
	FinishedAt *time.Time
	Result     job.Result
	Findings   int
	Error      string
}

// LastRun is the most recent run of the job, finished or not. ok is false if the job never ran.
func (s *Store) LastRun(ctx context.Context, name string) (r Run, ok bool, err error) {
	return s.one(ctx, `WHERE job = $1 ORDER BY started_at DESC, id DESC LIMIT 1`, name)
}

// LastSuccess is the most recent run that finished ok or warn (the job completed without finding a
// critical problem). Invariant 2: the dashboard shows it apart from LastRun.
func (s *Store) LastSuccess(ctx context.Context, name string) (r Run, ok bool, err error) {
	return s.one(ctx, `WHERE job = $1 AND result IN ('ok','warn') ORDER BY started_at DESC, id DESC LIMIT 1`, name)
}

func (s *Store) one(ctx context.Context, tail string, args ...any) (r Run, ok bool, err error) {
	var res, msg *string
	err = s.pool.QueryRow(ctx,
		`SELECT id, job, started_at, finished_at, result, findings, error FROM job_runs `+tail, args...).
		Scan(&r.ID, &r.Job, &r.StartedAt, &r.FinishedAt, &res, &r.Findings, &msg)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	if res != nil {
		r.Result = job.Result(*res)
	}
	if msg != nil {
		r.Error = *msg
	}
	return r, true, nil
}
