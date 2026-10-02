package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNonceGapOpen: a Nonce Gap is open, so no new Hot Wallet nonce may be
	// allocated, regardless of caller (withdrawal-flow.md invariant #7).
	ErrNonceGapOpen = errors.New("nonce gap open")
	// ErrIdempotencyKeyReused: the key was already used for a different request (or by another caller).
	ErrIdempotencyKeyReused = errors.New("idempotency key already used for a different request")
	// ErrKeyVoided: the key was voided (invariant #22): it is never signed, and allocates no nonce.
	ErrKeyVoided = errors.New("idempotency key was voided")
)

// HotSignature is a stored Hot Wallet signature: what a replay returns.
type HotSignature struct {
	Nonce       uint64
	SignedRawTx []byte
	CallerCN    *string // nil for a row signed before caller scoping existed
}

// LookupHotSignature returns the stored signature for an idempotency key, ErrNotFound if none, or
// ErrIdempotencyKeyReused if requestHash differs or the key was signed for a different caller.
func (s *Store) LookupHotSignature(ctx context.Context, caller, key, requestHash string) (HotSignature, error) {
	return lookupHotSignature(ctx, s.pool, caller, key, requestHash)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func lookupHotSignature(ctx context.Context, q queryRower, caller, key, requestHash string) (HotSignature, error) {
	var (
		sig  HotSignature
		hash string
		n    int64
	)
	err := q.QueryRow(ctx,
		`SELECT nonce, signed_raw_tx, request_hash, caller_cn FROM hot_wallet_signatures WHERE idempotency_key = $1`, key).
		Scan(&n, &sig.SignedRawTx, &hash, &sig.CallerCN)
	if errors.Is(err, pgx.ErrNoRows) {
		return HotSignature{}, ErrNotFound
	}
	if err != nil {
		return HotSignature{}, fmt.Errorf("reading hot wallet signature: %w", err)
	}
	if hash != requestHash || (sig.CallerCN != nil && *sig.CallerCN != caller) {
		return HotSignature{}, ErrIdempotencyKeyReused
	}
	sig.Nonce = uint64(n)
	return sig, nil
}

// SignHotWallet atomically claims the next Hot Wallet nonce for chainID, has
// sign produce the signed tx for it, and stores (key -> nonce, signed tx) — all
// in one DB transaction, so a failed signature never consumes a nonce and a
// retried key never claims a second one (invariants #6, #16).
//
//   - The chain's nonce row is locked FOR UPDATE first, serialising every signer
//     for the chain; the idempotency lookup happens under that lock, so two
//     concurrent calls with the same key can't both allocate.
//   - seedNonce supplies the starting nonce (the Hot Wallet's live pending nonce)
//     the first time a chain is used; it is called at most once per chain.
//   - An open Nonce Gap refuses new allocation (ErrNonceGapOpen) but still lets
//     an already-stored key replay.
func (s *Store) SignHotWallet(
	ctx context.Context, chainID int64, caller, key, requestHash string,
	seedNonce func(context.Context) (uint64, error),
	sign func(nonce uint64) ([]byte, error),
) (HotSignature, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return HotSignature{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	next, frozen, err := lockNonceRow(ctx, tx, chainID, seedNonce)
	if err != nil {
		return HotSignature{}, err
	}

	if existing, err := lookupHotSignature(ctx, tx, caller, key, requestHash); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return HotSignature{}, err
	}

	// Under the nonce-row lock a void cannot race this: either it committed first (refused here,
	// allocating nothing) or it will find the signature this call commits (invariant #22).
	var voided bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hot_wallet_voids WHERE idempotency_key = $1)`, key).Scan(&voided); err != nil {
		return HotSignature{}, fmt.Errorf("reading void tombstone: %w", err)
	}
	if voided {
		return HotSignature{}, ErrKeyVoided
	}

	if frozen != nil {
		return HotSignature{}, ErrNonceGapOpen
	}

	raw, err := sign(uint64(next))
	if err != nil {
		return HotSignature{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, nonce, request_hash, signed_raw_tx, caller_cn)
		 VALUES ($1, $2, $3, $4, $5, $6)`, key, chainID, next, requestHash, raw, caller); err != nil {
		return HotSignature{}, fmt.Errorf("storing signature: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE hot_wallet_nonces SET next_nonce = next_nonce + 1 WHERE chain_id = $1`, chainID); err != nil {
		return HotSignature{}, fmt.Errorf("advancing nonce: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return HotSignature{}, fmt.Errorf("committing tx: %w", err)
	}
	return HotSignature{Nonce: uint64(next), SignedRawTx: raw, CallerCN: &caller}, nil
}

// ErrNonceNotAllocated: a Nonce Gap was reported at a nonce this chain never allocated — a
// buggy or hostile report, refused rather than freezing on nonsense.
var ErrNonceNotAllocated = errors.New("nonce was never allocated")

// NonceFreeze is the effective freeze on a chain.
type NonceFreeze struct {
	ChainID   int64
	FromNonce uint64
	At        time.Time
}

// FreezeNonceGap records an open Nonce Gap at fromNonce for the chain; from then on
// SignHotWallet allocates nothing new (invariant #7). If a gap is already open the lower nonce
// wins, so a repeat is a no-op and a lower report only widens it (#8). Returns the effective
// freeze. ErrNotFound if the chain has never signed anything (no gap can exist);
// ErrNonceNotAllocated if fromNonce >= next_nonce.
func (s *Store) FreezeNonceGap(ctx context.Context, chainID int64, fromNonce uint64) (NonceFreeze, error) {
	var from int64
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE hot_wallet_nonces
		   SET frozen_from_nonce = LEAST(COALESCE(frozen_from_nonce, $2), $2),
		       frozen_at = COALESCE(frozen_at, now())
		 WHERE chain_id = $1 AND $2 < next_nonce
		RETURNING frozen_from_nonce, frozen_at`, chainID, int64(fromNonce)).Scan(&from, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hot_wallet_nonces WHERE chain_id = $1)`, chainID).Scan(&exists); err != nil {
			return NonceFreeze{}, err
		}
		if !exists {
			return NonceFreeze{}, ErrNotFound
		}
		return NonceFreeze{}, ErrNonceNotAllocated
	}
	if err != nil {
		return NonceFreeze{}, fmt.Errorf("freezing nonce gap: %w", err)
	}
	return NonceFreeze{ChainID: chainID, FromNonce: uint64(from), At: at}, nil
}

// ResolveNonceGap clears the freeze flag (operator action, invariant #9). It does
// not touch next_nonce: what the sequence should look like afterwards depends on
// how the operator resolved the gap, which the design docs don't yet specify.
func (s *Store) ResolveNonceGap(ctx context.Context, chainID int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hot_wallet_nonces SET frozen_from_nonce = NULL, frozen_at = NULL WHERE chain_id = $1`, chainID)
	if err != nil {
		return fmt.Errorf("resolving nonce gap: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// lockNonceRow locks the chain's nonce row FOR UPDATE — the one lock every signer, voider and
// replacer of that chain serialises on — creating it from seedNonce the first time the chain is used.
func lockNonceRow(ctx context.Context, tx pgx.Tx, chainID int64, seedNonce func(context.Context) (uint64, error)) (next int64, frozen *int64, err error) {
	lock := func() error {
		return tx.QueryRow(ctx,
			`SELECT next_nonce, frozen_from_nonce FROM hot_wallet_nonces WHERE chain_id = $1 FOR UPDATE`, chainID).
			Scan(&next, &frozen)
	}
	err = lock()
	if errors.Is(err, pgx.ErrNoRows) {
		if seedNonce == nil {
			return 0, nil, ErrNotFound
		}
		seed, serr := seedNonce(ctx)
		if serr != nil {
			return 0, nil, fmt.Errorf("seeding hot wallet nonce: %w", serr)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO hot_wallet_nonces (chain_id, next_nonce) VALUES ($1, $2) ON CONFLICT (chain_id) DO NOTHING`,
			chainID, int64(seed)); err != nil {
			return 0, nil, fmt.Errorf("creating nonce row: %w", err)
		}
		err = lock()
	}
	if err != nil {
		return 0, nil, fmt.Errorf("locking nonce row: %w", err)
	}
	return next, frozen, nil
}
