package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SealedSeed64 returns the sealed 64-byte Seed of a role (migration 000008), or ErrNotFound while it has not been written.
// Only key-admin reads it from this process: sweep-signer has its own read-only role and store.
func (s *Store) SealedSeed64(ctx context.Context, role string) ([]byte, error) {
	var blob []byte
	err := s.pool.QueryRow(ctx, `SELECT sealed_seed64 FROM wallet_seeds WHERE seed_role = $1`, role).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(blob) == 0) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s sealed_seed64: %w", role, err)
	}
	return blob, nil
}

// InitSeed64 stores a brand-new Seed for a role in the 64-byte format only (`key-admin init-seed`): no mnemonic-format blob
// exists. ErrAlreadyInitialized if the role already has a row.
func (s *Store) InitSeed64(ctx context.Context, role string, blob []byte) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO wallet_seeds (seed_role, sealed_seed, sealed_seed64) VALUES ($1, NULL, $2) ON CONFLICT (seed_role) DO NOTHING`, role, blob)
	if err != nil {
		return fmt.Errorf("inserting %s seed: %w", role, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInitialized
	}
	return nil
}

// SetSealedSeed64 writes the converted Seed next to the existing mnemonic-format blob (`key-admin convert-seed`). It writes
// only where sealed_seed64 is still NULL: a conversion is never silently overwritten. The old blob is not touched.
func (s *Store) SetSealedSeed64(ctx context.Context, role string, blob []byte) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE wallet_seeds SET sealed_seed64 = $2 WHERE seed_role = $1 AND sealed_seed64 IS NULL`, role, blob)
	if err != nil {
		return fmt.Errorf("writing %s sealed_seed64: %w", role, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyInitialized
	}
	return nil
}
