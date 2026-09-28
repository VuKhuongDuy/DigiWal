// Package storage is wallet-service's Postgres access layer: the one
// master wallet row (encrypted seed + next derivation index) and the
// per-user address assignments. This is the source of truth — Redis
// only ever caches what's here.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS master_wallet (
	id             INT PRIMARY KEY DEFAULT 1 CHECK (id = 1), -- singleton row
	sealed_seed    BYTEA NOT NULL,
	next_index     INT NOT NULL DEFAULT 0,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wallet_addresses (
	user_id          TEXT NOT NULL,
	chain            TEXT NOT NULL DEFAULT 'evm',
	derivation_index INT NOT NULL,
	address          TEXT NOT NULL UNIQUE,
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (user_id, chain)
);
`

var ErrNotFound = errors.New("not found")
var ErrAlreadyInitialized = errors.New("master wallet already initialized")

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
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

// InitMasterWallet stores the sealed seed exactly once. Returns
// ErrAlreadyInitialized on a second call — this is a one-time,
// operator-driven action, not something that silently no-ops.
func (s *Store) InitMasterWallet(ctx context.Context, sealedSeed []byte) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO master_wallet (id, sealed_seed) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`,
		sealedSeed)
	if err != nil {
		return fmt.Errorf("inserting master wallet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInitialized
	}
	return nil
}

// SealedSeed returns the stored ciphertext, or ErrNotFound if the
// master wallet hasn't been initialized yet.
func (s *Store) SealedSeed(ctx context.Context) ([]byte, error) {
	var seed []byte
	err := s.pool.QueryRow(ctx, `SELECT sealed_seed FROM master_wallet WHERE id = 1`).Scan(&seed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading master wallet: %w", err)
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

// AllocateIndex atomically claims the next derivation index for a new
// address and returns it. The actual address computation happens
// outside the transaction (it's pure math, no need to hold a DB lock
// for it) — CreateAddress below does both steps together.
func (s *Store) allocateIndex(ctx context.Context, tx pgx.Tx) (uint32, error) {
	var idx uint32
	err := tx.QueryRow(ctx,
		`UPDATE master_wallet SET next_index = next_index + 1 WHERE id = 1 RETURNING next_index - 1`).
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
