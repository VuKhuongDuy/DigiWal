package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"digiwal/services/wallet-service/internal/xpub"
)

// XPubRow is a wallet_xpubs row (key-custody-hsm.md M3a). The xpub is public but still never returned by an API or logged.
type XPubRow struct {
	Chain       string
	Path        string
	Depth       int
	XPub        string
	Attestation []byte
	NextIndex   uint32
	CreatedAt   time.Time
}

// Record is the part of the row the attestation covers.
func (r XPubRow) Record() xpub.Record {
	return xpub.Record{Chain: r.Chain, Path: r.Path, XPub: r.XPub, Attestation: r.Attestation}
}

// XPub returns the chain's xpub row, or ErrNotFound.
func (s *Store) XPub(ctx context.Context, chain string) (XPubRow, error) {
	var r XPubRow
	var next int32
	err := s.pool.QueryRow(ctx,
		`SELECT chain, path, depth, xpub, attestation, next_index, created_at FROM wallet_xpubs WHERE chain = $1`, chain).
		Scan(&r.Chain, &r.Path, &r.Depth, &r.XPub, &r.Attestation, &next, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return XPubRow{}, ErrNotFound
	}
	if err != nil {
		return XPubRow{}, fmt.Errorf("reading wallet_xpubs: %w", err)
	}
	r.NextIndex = uint32(next)
	return r, nil
}

// ErrXPubExists: a different xpub row is already there (rotation is M3c, not an overwrite).
var ErrXPubExists = errors.New("wallet_xpubs already has a row for this chain")

// InsertXPub stores the row (used by key-admin migrate-xpub only). It never overwrites: an identical xpub already stored
// returns nil (re-running the migration is a no-op), a different one returns ErrXPubExists.
func (s *Store) InsertXPub(ctx context.Context, r XPubRow) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO wallet_xpubs (chain, path, depth, xpub, attestation, next_index) VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (chain) DO NOTHING`, r.Chain, r.Path, r.Depth, r.XPub, r.Attestation, int32(r.NextIndex))
	if err != nil {
		return fmt.Errorf("inserting wallet_xpubs: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := s.XPub(ctx, r.Chain)
	if err != nil {
		return err
	}
	if cur.XPub == r.XPub && cur.Path == r.Path {
		return nil
	}
	return ErrXPubExists
}

// AddressRow is one wallet_addresses row (key-admin's reproduction proof).
type AddressRow struct {
	UserID  string
	Chain   string
	Index   uint32
	Address string
}

// AllAddresses lists every wallet_addresses row of a chain, by derivation index.
func (s *Store) AllAddresses(ctx context.Context, chain string) ([]AddressRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, chain, derivation_index, address FROM wallet_addresses WHERE chain = $1 ORDER BY derivation_index`, chain)
	if err != nil {
		return nil, fmt.Errorf("listing wallet_addresses: %w", err)
	}
	defer rows.Close()
	var out []AddressRow
	for rows.Next() {
		var a AddressRow
		var idx int32
		if err := rows.Scan(&a.UserID, &a.Chain, &idx, &a.Address); err != nil {
			return nil, err
		}
		a.Index = uint32(idx)
		out = append(out, a)
	}
	return out, rows.Err()
}

// UserDerivationNextIndex is wallet_seeds.next_index of the user_derivation seed (key-admin reads it once to start the xpub
// counter where the seed-based counter stopped). The ISSUING path never calls this.
func (s *Store) UserDerivationNextIndex(ctx context.Context) (uint32, error) {
	var n int32
	err := s.pool.QueryRow(ctx, `SELECT next_index FROM wallet_seeds WHERE seed_role = $1`, RoleUserDerivation).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading wallet_seeds.next_index: %w", err)
	}
	return uint32(n), nil
}

// CreateAddressFromXPub claims the next derivation index from wallet_xpubs.next_index (never wallet_seeds) and records the
// address issue derives for it, atomically. The UPDATE ... RETURNING locks the chain's row, so concurrent issuances queue and
// each gets a distinct index; the row it returns (xpub, path, attestation) is the one issue must verify and derive from, inside
// the same transaction. If issue fails (the attestation does not verify) the transaction rolls back and the index is not consumed.
// ErrNotFound means no wallet_xpubs row exists for the chain.
func (s *Store) CreateAddressFromXPub(ctx context.Context, userID, chain string, issue func(rec xpub.Record, index uint32) (string, error)) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var rec xpub.Record
	var idx int32
	err = tx.QueryRow(ctx,
		`UPDATE wallet_xpubs SET next_index = next_index + 1 WHERE chain = $1
		 RETURNING chain, path, xpub, attestation, next_index - 1`, chain).Scan(&rec.Chain, &rec.Path, &rec.XPub, &rec.Attestation, &idx)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("allocating derivation index from wallet_xpubs: %w", err)
	}
	addr, err := issue(rec, uint32(idx))
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallet_addresses (user_id, chain, derivation_index, address) VALUES ($1, $2, $3, $4)`,
		userID, chain, idx, addr); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return "", fmt.Errorf("recording wallet address (duplicate user or address): %w", err)
		}
		return "", fmt.Errorf("recording wallet address: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing tx: %w", err)
	}
	return addr, nil
}
