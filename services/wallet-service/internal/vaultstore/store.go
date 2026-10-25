// Package vaultstore is key-admin's access to vault.sealed_secrets (infra/postgres/vault.sql, key-custody-hsm.md invariants 18 and 19),
// through the role svc_keyadmin. wallet-service never imports this package and its database role has no access to the schema at all.
// The store holds ciphertext only: the plaintext is opened inside hsm-gateway, and a row here is useless without the KEK in the token.
package vaultstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row statuses (invariant 19).
const (
	Pending = "pending"
	Active  = "active"
	Retired = "retired"
)

// ErrNotFound: no such row.
var ErrNotFound = errors.New("vault: not found")

// ErrExists: the (role, version) is already there.
var ErrExists = errors.New("vault: that role and version already exist")

// Row is one Sealed Secret.
type Row struct {
	Role       string
	Version    int
	Status     string
	Purpose    string
	KEKVersion int
	WrappedDEK []byte
	Nonce      []byte
	Ciphertext []byte
	AAD        []byte
	CreatedAt  time.Time
}

// KEKLabel is the token label of the row's KEK.
func (r Row) KEKLabel() string { return fmt.Sprintf("kek-v%d", r.KEKVersion) }

// Store is the connection.
type Store struct{ pool *pgxpool.Pool }

// Connect opens the pool as svc_keyadmin. passwordFile, if set, supplies the password from a secret FILE so it is never in the DSN.
func Connect(ctx context.Context, dsn, passwordFile string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing VAULT_DSN: %w", err)
	}
	if passwordFile != "" {
		raw, err := os.ReadFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("reading the vault database password file: %w", err)
		}
		cfg.ConnConfig.Password = strings.TrimSpace(string(raw))
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to the vault database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging the vault database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// New wraps an existing pool (tests).
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

const cols = `role, version, status, purpose, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad, created_at`

func scan(r pgx.Row) (Row, error) {
	var x Row
	err := r.Scan(&x.Role, &x.Version, &x.Status, &x.Purpose, &x.KEKVersion, &x.WrappedDEK, &x.Nonce, &x.Ciphertext, &x.AAD, &x.CreatedAt)
	return x, err
}

// List returns every version of a role, ascending.
func (s *Store) List(ctx context.Context, role string) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM vault.sealed_secrets WHERE role = $1 ORDER BY version`, role)
	if err != nil {
		return nil, fmt.Errorf("reading vault.sealed_secrets: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		x, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Insert stores a row. A new row is Pending unless the caller says otherwise (only pending rows are ever inserted by key-admin).
func (s *Store) Insert(ctx context.Context, r Row) error {
	if r.Status == "" {
		r.Status = Pending
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO vault.sealed_secrets (role, version, status, purpose, kek_label, kek_version, wrapped_dek, gcm_nonce, ciphertext, aad)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		r.Role, r.Version, r.Status, r.Purpose, r.KEKLabel(), r.KEKVersion, r.WrappedDEK, r.Nonce, r.Ciphertext, r.AAD)
	if err != nil {
		return fmt.Errorf("inserting into vault.sealed_secrets: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	return nil
}

// Activate is the flip, ONE transaction: the current active row of the role (if any) becomes retired and (role, version) becomes active.
// From pending (rotation) or retired (rollback). Activating the row that is already active is a no-op.
func (s *Store) Activate(ctx context.Context, role string, version int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM vault.sealed_secrets WHERE role = $1 AND version = $2 FOR UPDATE`, role, version).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status == Active {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE vault.sealed_secrets SET status = 'retired' WHERE role = $1 AND status = 'active'`, role); err != nil {
		return fmt.Errorf("retiring the active row: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE vault.sealed_secrets SET status = 'active' WHERE role = $1 AND version = $2`, role, version); err != nil {
		return fmt.Errorf("activating version %d: %w", version, err)
	}
	return tx.Commit(ctx)
}

// DeletePending removes an inert pending row (an interrupted rotation). The guard trigger refuses any other status.
func (s *Store) DeletePending(ctx context.Context, role string, version int) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vault.sealed_secrets WHERE role = $1 AND version = $2 AND status = 'pending'`, role, version)
	if err != nil {
		return fmt.Errorf("deleting the pending row: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
