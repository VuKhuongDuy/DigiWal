// Package store is sweep-signer's READ-ONLY view of the custody database, through the least-privilege role svc_sweepsigner
// (infra/postgres/sweep-signer-role.sql and vault.sql): SELECT on four columns of wallet_addresses, two of wallet_xpubs and the view
// vault.active_secrets (the active Sealed Secret: ciphertext only, opened by hsm-gateway). It cannot write, cannot read wallet_seeds at
// all, and cannot see anything else in the database. Nothing here runs a migration.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/services/sweep-signer/internal/vault"
)

// ErrNotFound: no such row.
var ErrNotFound = errors.New("not found")

// ChainEVM is the one chain family (wallet_addresses.chain default, wallet_xpubs CHECK).
const ChainEVM = "evm"

// Issued is one wallet_addresses row: the index a user's deposit address was issued at.
type Issued struct {
	UserID  string
	Index   uint32
	Address string // EIP-55 as stored
}

// Store reads the custody database.
type Store struct{ pool *pgxpool.Pool }

// Connect opens the pool. passwordFile, if set, supplies the role's password from a secret FILE (never the DSN, the
// environment or a compose file); the DSN then carries no password.
func Connect(ctx context.Context, dsn, passwordFile string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing POSTGRES_DSN: %w", err)
	}
	if passwordFile != "" {
		raw, err := os.ReadFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("reading the database password file: %w", err)
		}
		cfg.ConnConfig.Password = strings.TrimSpace(string(raw))
	}
	// Belt and braces beside the role's grants: every connection is read-only at the session level.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Issued returns the user's issued address row for the chain family, or ErrNotFound.
func (s *Store) Issued(ctx context.Context, userID, chain string) (Issued, error) {
	var r Issued
	r.UserID = userID
	err := s.pool.QueryRow(ctx,
		`SELECT derivation_index, address FROM wallet_addresses WHERE user_id = $1 AND chain = $2`, userID, chain).
		Scan(&r.Index, &r.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrNotFound
	}
	if err != nil {
		return Issued{}, fmt.Errorf("reading wallet_addresses: %w", err)
	}
	return r, nil
}

// IssuedAfter returns up to limit rows with derivation_index > after, ascending (the audit's cursor walk).
func (s *Store) IssuedAfter(ctx context.Context, chain string, after int64, limit int) ([]Issued, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, derivation_index, address FROM wallet_addresses
		  WHERE chain = $1 AND derivation_index > $2 ORDER BY derivation_index LIMIT $3`, chain, after, limit)
	if err != nil {
		return nil, fmt.Errorf("reading wallet_addresses: %w", err)
	}
	defer rows.Close()
	var out []Issued
	for rows.Next() {
		var r Issued
		if err := rows.Scan(&r.UserID, &r.Index, &r.Address); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveSecret is the ACTIVE Sealed Secret of role user_derivation, read through the view vault.active_secrets (granted by
// infra/postgres/vault.sql): ciphertext only, opened by hsm-gateway. ErrNotFound while the environment has not been initialised
// (`key-admin init`) or while an init/rotation has not flipped a row to active yet.
func (s *Store) ActiveSecret(ctx context.Context) (vault.Row, error) {
	var r vault.Row
	err := s.pool.QueryRow(ctx,
		`SELECT role, purpose, version, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad FROM vault.active_secrets WHERE role = 'user_derivation'`).
		Scan(&r.Role, &r.Purpose, &r.Version, &r.KEKVersion, &r.WrappedDEK, &r.Nonce, &r.Ciphertext, &r.AAD)
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.Row{}, ErrNotFound
	}
	if err != nil {
		return vault.Row{}, fmt.Errorf("reading the active sealed secret: %w", err)
	}
	return r, nil
}

// AccountXPub is the stored account xpub, or ErrNotFound before `key-admin migrate-xpub` has run.
func (s *Store) AccountXPub(ctx context.Context) (string, error) {
	var x string
	err := s.pool.QueryRow(ctx, `SELECT xpub FROM wallet_xpubs WHERE chain = 'evm'`).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading wallet_xpubs: %w", err)
	}
	return x, nil
}
