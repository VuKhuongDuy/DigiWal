// Package storage is wallet-service's Postgres access layer: the sealed
// seeds (one per role, plus the user-derivation index counter) and the
// per-user address assignments. Schema lives in ../../migrations. This is the source of truth — Redis
// only ever caches what's here.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/services/wallet-service/migrations"
)

// Seed roles (wallet-tiering.md invariant #1): the seed deriving user
// deposit addresses is never the Hot Wallet's key.
const (
	RoleUserDerivation = "user_derivation"
	RoleHotWallet      = "hot_wallet"
)

var ErrNotFound = errors.New("not found")
var ErrAlreadyInitialized = errors.New("seed already initialized")

type Store struct {
	pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	if err := MigrateUp(dsn); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// NewMigrator returns a golang-migrate instance over the embedded
// migrations for the given Postgres DSN. Callers must Close it.
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

// MigrateUp applies every pending migration. No-op when up to date.
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

func (s *Store) Close() {
	s.pool.Close()
}

// PoolStat is the connection pool's state, for the saturation metrics.
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }

// InitSeed stores the sealed seed for a role exactly once. Returns
// ErrAlreadyInitialized on a second call — this is a one-time,
// operator-driven action, not something that silently no-ops.
func (s *Store) InitSeed(ctx context.Context, role string, sealedSeed []byte) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO wallet_seeds (seed_role, sealed_seed) VALUES ($1, $2) ON CONFLICT (seed_role) DO NOTHING`,
		role, sealedSeed)
	if err != nil {
		return fmt.Errorf("inserting %s seed: %w", role, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInitialized
	}
	return nil
}

// SealedSeed returns the stored ciphertext for a role, or ErrNotFound
// if that seed hasn't been initialized yet.
func (s *Store) SealedSeed(ctx context.Context, role string) ([]byte, error) {
	var seed []byte
	err := s.pool.QueryRow(ctx, `SELECT sealed_seed FROM wallet_seeds WHERE seed_role = $1`, role).Scan(&seed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s seed: %w", role, err)
	}
	return seed, nil
}

// AddressFor returns the user's existing address for a chain, or
// ErrNotFound.
func (s *Store) AddressFor(ctx context.Context, userID, chain string) (string, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT address FROM wallet_addresses WHERE user_id = $1 AND chain = $2`, userID, chain).
		Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading wallet address: %w", err)
	}
	return addr, nil
}

// UserForAddress is the reverse of AddressFor: which user, and on which chain, owns an
// address. ErrNotFound if it is not one of ours. The lookup is by the exact stored
// spelling (EIP-55 checksum, as CreateAddress stores it); callers normalize first.
func (s *Store) UserForAddress(ctx context.Context, address string) (userID, chain string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT user_id, chain FROM wallet_addresses WHERE address = $1`, address).Scan(&userID, &chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("reading address owner: %w", err)
	}
	return userID, chain, nil
}

// DerivationIndexFor returns the BIP44 index of the user's address, or ErrNotFound.
func (s *Store) DerivationIndexFor(ctx context.Context, userID, chain string) (uint32, error) {
	var idx uint32
	err := s.pool.QueryRow(ctx,
		`SELECT derivation_index FROM wallet_addresses WHERE user_id = $1 AND chain = $2`, userID, chain).Scan(&idx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading derivation index: %w", err)
	}
	return idx, nil
}

// AllocateIndex atomically claims the next derivation index for a new
// address and returns it. The actual address computation happens
// outside the transaction (it's pure math, no need to hold a DB lock
// for it) — CreateAddress below does both steps together.
func (s *Store) allocateIndex(ctx context.Context, tx pgx.Tx) (uint32, error) {
	var idx uint32
	err := tx.QueryRow(ctx,
		`UPDATE wallet_seeds SET next_index = next_index + 1 WHERE seed_role = $1 RETURNING next_index - 1`, RoleUserDerivation).
		Scan(&idx)
	if err != nil {
		return 0, fmt.Errorf("allocating derivation index: %w", err)
	}
	return idx, nil
}

// CreateAddress claims the next derivation index and records the
// address deriveFn computes for it, atomically — so two concurrent
// requests for the same user can never end up claiming (and silently
// discarding) two different indices. deriveFn is called once, inside
// the transaction, with the freshly allocated index.
func (s *Store) CreateAddress(ctx context.Context, userID, chain string, deriveFn func(index uint32) (string, error)) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	idx, err := s.allocateIndex(ctx, tx)
	if err != nil {
		return "", err
	}
	addr, err := deriveFn(idx)
	if err != nil {
		return "", fmt.Errorf("deriving address for index %d: %w", idx, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallet_addresses (user_id, chain, derivation_index, address) VALUES ($1, $2, $3, $4)`,
		userID, chain, idx, addr); err != nil {
		return "", fmt.Errorf("recording wallet address: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing tx: %w", err)
	}
	return addr, nil
}
