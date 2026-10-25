// Package store is api-gateway's Postgres: users and API keys (docs/design/identity-auth.md).
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"digiwal/services/api-gateway/migrations"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// KeyPrefix marks a DigiWal API key, so a leaked one is recognizable to secret scanners.
const KeyPrefix = "dw_"

// ErrInvalidKey means the presented API key matches no live key. It never says which of
// unknown / revoked / malformed it was.
var ErrInvalidKey = errors.New("invalid api key")

type Store struct{ pool *pgxpool.Pool }

func Connect(ctx context.Context, dsn string) (*Store, error) { return ConnectWithPool(ctx, dsn, 0) }

// ConnectWithPool is Connect with an explicit pool size (0 keeps pgx's default of max(4, CPUs)). The size is a
// separate argument, not a pool_max_conns parameter in the DSN: the same DSN also goes to golang-migrate, which
// would send that parameter to the server as a runtime setting and be refused ("unrecognized configuration
// parameter"), so a service could not start.
func ConnectWithPool(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
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

// HashKey is the stored form of an API key. SHA-256 is enough because the key is 32 random
// bytes, not a human-chosen password (identity-auth.md Open Question #2).
func HashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// CreateUser makes a user and its first API key in one transaction. The plaintext key is
// returned here and nowhere else, ever (invariants #1, #2).
func (s *Store) CreateUser(ctx context.Context) (userID, apiKey string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generating api key: %w", err)
	}
	apiKey = KeyPrefix + hex.EncodeToString(raw)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id::text`).Scan(&userID); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_keys (user_id, key_hash) VALUES ($1, $2)`, userID, HashKey(apiKey)); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return userID, apiKey, nil
}

// Authenticate resolves an API key to its user with a live lookup, so a revocation takes
// effect on the very next request (invariant #6).
func (s *Store) Authenticate(ctx context.Context, apiKey string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id::text FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, HashKey(apiKey)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidKey
	}
	return userID, err
}

// RevokeKey revokes the key. No HTTP endpoint exposes it (identity-auth.md designs none):
// it is for an operator tool and for tests. Reports whether a live key was revoked.
func (s *Store) RevokeKey(ctx context.Context, apiKey string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE key_hash = $1 AND revoked_at IS NULL`, HashKey(apiKey))
	return tag.RowsAffected() == 1, err
}

// Pool exposes the pool for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// CountUsers is the number of users ever created. Users are never deleted, so it only grows.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}
