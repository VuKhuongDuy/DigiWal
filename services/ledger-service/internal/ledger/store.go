// Package ledger is the Ledger bounded context (docs/design/ledger.md): accounts,
// immutable entries, the Asset Registry mirror, and the one operation everything
// else calls into — Apply.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/pkg/metrics"
	"digiwal/services/ledger-service/migrations"
)

type Store struct {
	pool *pgxpool.Pool
	// afterChange, when set, is told which users' balances a committed transaction changed (the read cache deletes their entries,
	// docs/design/read-cache.md). Called only after Commit succeeded; every writer to accounts.total_balance or locked_balance must call changed.
	afterChange func(ctx context.Context, userIDs []string)
}

// OnBalanceChange registers the hook above. Not safe to call while requests are being served.
func (s *Store) OnBalanceChange(f func(ctx context.Context, userIDs []string)) { s.afterChange = f }

func (s *Store) changed(ctx context.Context, userIDs ...string) {
	if s.afterChange != nil && len(userIDs) > 0 {
		s.afterChange(ctx, userIDs)
	}
}

// Connect opens the pool and applies any pending migrations.
func Connect(ctx context.Context, dsn string) (*Store, error) { return ConnectWithPool(ctx, dsn, 0) }

// ConnectWithPool is Connect with an explicit pool size (0 keeps pgx's default of max(4, CPUs)). The size is a
// separate argument, not a pool_max_conns parameter in the DSN: the same DSN also goes to golang-migrate, which
// would send that parameter to the server as a runtime setting and be refused ("unrecognized configuration
// parameter"), so the service could not start.
func ConnectWithPool(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	return ConnectWithPoolMetrics(ctx, dsn, maxConns, nil)
}

// ConnectWithPoolMetrics is ConnectWithPool that also feeds m (nil: not measured) with the time to get a connection, to hold one and to run a statement.
func ConnectWithPoolMetrics(ctx context.Context, dsn string, maxConns int32, m *metrics.DB) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	attach(cfg, m)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

// PoolStat is the connection pool's state, for the saturation metrics.
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }

// NewMigrator returns a golang-migrate instance over the embedded migrations.
func NewMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("loading embedded migrations: %w", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("creating migrator: %w", err)
	}
	return m, nil
}

// MigrateUp applies every pending migration; a no-op when up to date.
func MigrateUp(dsn string) error {
	m, err := NewMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}
