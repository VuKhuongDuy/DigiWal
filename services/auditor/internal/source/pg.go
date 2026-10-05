package source

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is a read-only connection to one service's Postgres.
type PG struct {
	name string
	pool *pgxpool.Pool
}

// Querier is what rule code gets: reads only.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OpenPG connects with the session set read-only. The role (roles/) is the real barrier; this is the
// second one.
func OpenPG(ctx context.Context, name, dsn string) (*PG, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &PG{name: name, pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

func (p *PG) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := CheckSelect(sql); err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	return p.pool.Query(ctx, sql, args...)
}

func (p *PG) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := CheckSelect(sql); err != nil {
		return errRow{fmt.Errorf("%s: %w", p.name, err)}
	}
	return p.pool.QueryRow(ctx, sql, args...)
}

// Snapshot runs fn in one REPEATABLE READ, READ ONLY transaction, so related reads (the ledger checks)
// see one consistent state.
func (p *PG) Snapshot(ctx context.Context, fn func(Querier) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("%s: %w", p.name, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only: nothing to commit
	return fn(checked{name: p.name, tx: tx})
}

type checked struct {
	name string
	tx   pgx.Tx
}

func (c checked) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := CheckSelect(sql); err != nil {
		return nil, fmt.Errorf("%s: %w", c.name, err)
	}
	return c.tx.Query(ctx, sql, args...)
}

func (c checked) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := CheckSelect(sql); err != nil {
		return errRow{fmt.Errorf("%s: %w", c.name, err)}
	}
	return c.tx.QueryRow(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
