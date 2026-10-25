package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrNotCallers: the key exists but was signed for another caller (or before caller scoping
	// existed), so this caller may not void or replace it (withdrawal-flow.md invariant #22).
	ErrNotCallers = errors.New("key belongs to another caller")
	// ErrReplacementGap: replacement_seq is neither a stored one (a replay) nor the next one.
	ErrReplacementGap = errors.New("replacement_seq is not the next one")
	// ErrOtherHotKey: the key was signed by a different Hot address than the one signing now.
	ErrOtherHotKey = errors.New("key was signed by another Hot address")
	// ErrKeyCancelled: the latest version is a Nonce Filler; a Fee Bump must not undo it.
	ErrKeyCancelled = errors.New("key was cancelled by a Nonce Filler")
)

// Replacement kinds.
const (
	KindFeeBump = "fee_bump"
	KindCancel  = "cancel"
)

// Replacement is one re-signed version of a signed key: same nonce, higher fees.
type Replacement struct {
	Seq          int
	Nonce        uint64
	SignedRawTx  []byte
	TxHash       string
	MaxFeePerGas *big.Int
	MaxTip       *big.Int
	Kind         string
	CreatedAt    time.Time
}

// HotKey is a signed key and every replacement of it.
type HotKey struct {
	HotSignature
	Replacements []Replacement
}

func lookupReplacements(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, key string) ([]Replacement, error) {
	rows, err := q.Query(ctx, `SELECT replacement_seq, nonce, signed_raw_tx, tx_hash, max_fee_per_gas::text,
		max_priority_fee_per_gas::text, kind, created_at FROM hot_wallet_replacements
		WHERE idempotency_key = $1 ORDER BY replacement_seq`, key)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Replacement, error) {
		var out Replacement
		var n int64
		var fee, tip string
		if err := r.Scan(&out.Seq, &n, &out.SignedRawTx, &out.TxHash, &fee, &tip, &out.Kind, &out.CreatedAt); err != nil {
			return out, err
		}
		out.Nonce = uint64(n)
		var ok1, ok2 bool
		out.MaxFeePerGas, ok1 = new(big.Int).SetString(fee, 10)
		out.MaxTip, ok2 = new(big.Int).SetString(tip, 10)
		if !ok1 || !ok2 {
			return out, fmt.Errorf("unparseable fees %q/%q", fee, tip)
		}
		return out, nil
	})
}

// GetHotKey is GET /v1/sign/hot-wallet/{key}: the signature and its replacements, read-only.
// ErrNotFound when the key was never signed, or was signed for another caller (it must not even
// reveal that the key exists).
func (s *Store) GetHotKey(ctx context.Context, caller, key string) (HotKey, error) {
	var k HotKey
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT nonce, signed_raw_tx, caller_cn FROM hot_wallet_signatures WHERE idempotency_key = $1`, key).
		Scan(&n, &k.SignedRawTx, &k.CallerCN)
	if errors.Is(err, pgx.ErrNoRows) {
		return HotKey{}, ErrNotFound
	}
	if err != nil {
		return HotKey{}, fmt.Errorf("reading hot wallet signature: %w", err)
	}
	if k.CallerCN == nil || *k.CallerCN != caller {
		return HotKey{}, ErrNotFound
	}
	k.Nonce = uint64(n)
	if k.Replacements, err = lookupReplacements(ctx, s.pool, key); err != nil {
		return HotKey{}, fmt.Errorf("reading replacements: %w", err)
	}
	return k, nil
}

// VoidResult is VoidHotKey's outcome: Voided, or the signature that already exists.
type VoidResult struct {
	Voided bool
	Signed HotKey // set when !Voided
}

// VoidHotKey is POST .../void (invariant #22): under the same nonce-row lock signing takes, either
// the key already has a signature (returned, nothing voided) or a tombstone is written and every
// later sign for the key is refused (ErrKeyVoided) without allocating a nonce. Idempotent.
// ErrNotCallers if the key was signed (or already voided) for another caller.
func (s *Store) VoidHotKey(ctx context.Context, chainID int64, hot, caller, key string, seedNonce func(context.Context) (uint64, error)) (VoidResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VoidResult{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockNonceRow(ctx, tx, chainID, hot, seedNonce); err != nil {
		return VoidResult{}, err
	}

	var k HotKey
	var n int64
	err = tx.QueryRow(ctx, `SELECT nonce, signed_raw_tx, caller_cn FROM hot_wallet_signatures WHERE idempotency_key = $1`, key).
		Scan(&n, &k.SignedRawTx, &k.CallerCN)
	switch {
	case err == nil:
		if k.CallerCN == nil || *k.CallerCN != caller {
			return VoidResult{}, ErrNotCallers
		}
		k.Nonce = uint64(n)
		if k.Replacements, err = lookupReplacements(ctx, tx, key); err != nil {
			return VoidResult{}, err
		}
		return VoidResult{Signed: k}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return VoidResult{}, fmt.Errorf("reading hot wallet signature: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO hot_wallet_voids (idempotency_key, caller_cn, chain_id) VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING`, key, caller, chainID); err != nil {
		return VoidResult{}, fmt.Errorf("writing void tombstone: %w", err)
	}
	var owner string
	if err := tx.QueryRow(ctx, `SELECT caller_cn FROM hot_wallet_voids WHERE idempotency_key = $1`, key).Scan(&owner); err != nil {
		return VoidResult{}, err
	}
	if owner != caller {
		return VoidResult{}, ErrNotCallers
	}
	if err := tx.Commit(ctx); err != nil {
		return VoidResult{}, fmt.Errorf("committing tx: %w", err)
	}
	return VoidResult{Voided: true}, nil
}

// ReplaceInput is what a replacement is built from: the stored original and the latest version.
type ReplaceInput struct {
	Nonce       uint64
	OriginalRaw []byte
	LatestRaw   []byte // the original, or the latest replacement
	LatestKind  string // "" for the original
	Seq         int    // the sequence number being created
}

// Built is the new version a build callback returns.
type Built struct {
	Raw    []byte
	Hash   string
	MaxFee *big.Int
	MaxTip *big.Int
}

// ReplaceHotKey stores a re-signed version of a signed key, under the nonce-row lock; build makes
// the new version from the stored ones, so nothing a caller sends can change destination or amount.
//
// seq is the caller's replacement_seq: an existing one replays the stored replacement (idempotent),
// last+1 creates, anything else is ErrReplacementGap. seq 0 means "the next one" (operator).
// caller "" is the operator CLI: it may touch any key, and is not stopped by a Nonce Gap freeze
// (a filler is how a gap is repaired). A caller is stopped by a freeze covering the nonce
// (ErrNonceGapOpen), by another caller's key (ErrNotCallers) and by a Nonce Filler (ErrKeyCancelled).
// For kind=cancel an existing cancel is returned as it is, so retrying the command only re-sends it.
func (s *Store) ReplaceHotKey(ctx context.Context, chainID int64, hot, caller, key string, seq int, kind string,
	build func(ReplaceInput) (Built, error)) (Replacement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Replacement{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)
	_, frozen, err := lockNonceRow(ctx, tx, chainID, hot, nil)
	if err != nil {
		return Replacement{}, err
	}

	var n int64
	var orig []byte
	var owner *string
	var signedBy string
	err = tx.QueryRow(ctx, `SELECT nonce, signed_raw_tx, caller_cn, hot_address FROM hot_wallet_signatures WHERE idempotency_key = $1`, key).
		Scan(&n, &orig, &owner, &signedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Replacement{}, ErrNotFound
	}
	if err != nil {
		return Replacement{}, fmt.Errorf("reading hot wallet signature: %w", err)
	}
	if caller != "" && (owner == nil || *owner != caller) {
		return Replacement{}, ErrNotCallers
	}
	// A replacement is signed by the current Hot key: a key signed by another Hot address (a legacy
	// one, before a cutover) lives in another nonce domain and cannot be replaced by this key.
	if signedBy != hot {
		return Replacement{}, ErrOtherHotKey
	}
	if caller != "" && frozen != nil && n >= *frozen {
		return Replacement{}, ErrNonceGapOpen
	}

	reps, err := lookupReplacements(ctx, tx, key)
	if err != nil {
		return Replacement{}, fmt.Errorf("reading replacements: %w", err)
	}
	last := len(reps)
	latestRaw, latestKind := orig, ""
	if last > 0 {
		latestRaw, latestKind = reps[last-1].SignedRawTx, reps[last-1].Kind
	}
	switch {
	case seq < 0:
		return Replacement{}, ErrReplacementGap
	case seq >= 1 && seq <= last:
		return reps[seq-1], nil // a replay
	case seq > last+1:
		return Replacement{}, ErrReplacementGap
	}
	if kind == KindCancel && latestKind == KindCancel {
		return reps[last-1], nil
	}
	if latestKind == KindCancel {
		return Replacement{}, ErrKeyCancelled
	}

	b, err := build(ReplaceInput{Nonce: uint64(n), OriginalRaw: orig, LatestRaw: latestRaw, LatestKind: latestKind, Seq: last + 1})
	if err != nil {
		return Replacement{}, err
	}
	out := Replacement{Seq: last + 1, Nonce: uint64(n), SignedRawTx: b.Raw, TxHash: b.Hash, MaxFeePerGas: b.MaxFee, MaxTip: b.MaxTip, Kind: kind}
	if err := tx.QueryRow(ctx, `INSERT INTO hot_wallet_replacements
		(idempotency_key, replacement_seq, nonce, signed_raw_tx, tx_hash, max_fee_per_gas, max_priority_fee_per_gas, kind)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8) RETURNING created_at`,
		key, out.Seq, n, b.Raw, b.Hash, b.MaxFee.String(), b.MaxTip.String(), kind).Scan(&out.CreatedAt); err != nil {
		return Replacement{}, fmt.Errorf("storing replacement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Replacement{}, fmt.Errorf("committing tx: %w", err)
	}
	return out, nil
}
