package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
)

type LockStatus string

const (
	LockActive   LockStatus = "active"
	LockSettled  LockStatus = "settled"
	LockReleased LockStatus = "released"
)

var (
	ErrLockNotFound        = errors.New("lock not found")
	ErrLockAlreadySettled  = errors.New("lock already settled")
	ErrLockAlreadyReleased = errors.New("lock already released")
)

// Lock is a hold against a user's Available Balance (ledger.md "Lock").
type Lock struct {
	IdempotencyKey string
	Status         LockStatus
	Amount         *big.Int
}

// LockRequest asks to hold Amount of a user's Available Balance. The caller names
// the user and asset, never an account id: that is the ledger's own key.
type LockRequest struct {
	IdempotencyKey string // the Withdrawal Request's key, reused unchanged
	UserID         string
	AssetID        string
	Amount         *big.Int
}

func (r LockRequest) validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }
	switch {
	case r.IdempotencyKey == "" || len(r.IdempotencyKey) > 256:
		return bad("idempotency_key is required (max 256 chars)")
	case r.UserID == "":
		return bad("user_id is required")
	case !uuidRe.MatchString(strings.ToLower(r.AssetID)):
		return bad("asset_id is not a uuid")
	case r.Amount == nil || r.Amount.Sign() <= 0:
		return bad("amount must be positive")
	case r.Amount.Cmp(maxAmount) > 0:
		return bad("amount exceeds uint256")
	}
	return nil
}

// Lock reserves capacity for a withdrawal without moving any money: it posts no
// Entry and never touches total_balance, it only raises locked_balance (ledger.md
// invariants #1, #6). The check that Available Balance covers the amount is the same
// statement that takes the hold, so two concurrent locks on one account serialize on
// its row and can never over-reserve.
//
// Replaying a key returns the existing lock's current status (which may already be
// settled or released) and changes nothing; the same key with a different user, asset
// or amount is ErrIdempotencyKeyReused.
func (s *Store) Lock(ctx context.Context, r LockRequest) (Lock, error) {
	if err := r.validate(); err != nil {
		return Lock{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Lock{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Resolve (and on first touch create) the (user, asset) account. If the lock then
	// fails for lack of funds the whole transaction rolls back, account creation included.
	accountID, err := resolveAccount(ctx, tx, RefUser, r.UserID, r.AssetID)
	if err != nil {
		return Lock{}, err
	}

	var lockID string
	err = tx.QueryRow(ctx, `
		INSERT INTO locks (idempotency_key, account_id, amount) VALUES ($1, $2, $3::numeric)
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`, r.IdempotencyKey, accountID, r.Amount.String()).Scan(&lockID)
	if errors.Is(err, pgx.ErrNoRows) { // this key was locked before
		var (
			prevAccount, status, amount string
		)
		if err := tx.QueryRow(ctx, `SELECT account_id, status, amount::text FROM locks WHERE idempotency_key = $1`, r.IdempotencyKey).
			Scan(&prevAccount, &status, &amount); err != nil {
			return Lock{}, fmt.Errorf("reading existing lock: %w", err)
		}
		prev, _ := new(big.Int).SetString(amount, 10)
		if prevAccount != accountID || prev.Cmp(r.Amount) != 0 {
			return Lock{}, ErrIdempotencyKeyReused
		}
		return Lock{IdempotencyKey: r.IdempotencyKey, Status: LockStatus(status), Amount: prev}, nil
	}
	if err != nil {
		return Lock{}, fmt.Errorf("inserting lock: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE accounts SET locked_balance = locked_balance + $2::numeric
		WHERE id = $1 AND total_balance - locked_balance >= $2::numeric`, accountID, r.Amount.String())
	if err != nil {
		return Lock{}, fmt.Errorf("locking balance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Lock{}, ErrInsufficientFunds // rolls back the lock row too
	}
	if err := tx.Commit(ctx); err != nil {
		return Lock{}, fmt.Errorf("committing tx: %w", err)
	}
	return Lock{IdempotencyKey: r.IdempotencyKey, Status: LockActive, Amount: r.Amount}, nil
}

// SettleResult is the outcome of Settle.
type SettleResult struct {
	IdempotencyKey      string
	LedgerTransactionID string
}

// Settle turns a Lock into the real debit: the withdrawal happened. The lock-status
// guard, both balance updates and both entry inserts are ONE Postgres transaction and
// it deliberately does not go through Apply: a Lock's amount was already reserved out
// of Available Balance, so Settle can never fail for lack of funds (invariant #7), and
// routing it through Apply would debit total_balance twice.
//
// Settling an already-settled lock is a no-op that returns the same result; settling a
// released lock is ErrLockAlreadyReleased (the caller must not treat that as debited).
func (s *Store) Settle(ctx context.Context, key string) (SettleResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SettleResult{}, fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var accountID, amountText string
	err = tx.QueryRow(ctx, `
		UPDATE locks SET status = 'settled', resolved_at = now()
		WHERE idempotency_key = $1 AND status = 'active'
		RETURNING account_id, amount::text`, key).Scan(&accountID, &amountText)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return s.resolvedLock(ctx, key, LockSettled)
	}
	if err != nil {
		return SettleResult{}, fmt.Errorf("resolving lock: %w", err)
	}

	var assetID string
	if err := tx.QueryRow(ctx, `SELECT asset_id FROM accounts WHERE id = $1`, accountID).Scan(&assetID); err != nil {
		return SettleResult{}, fmt.Errorf("reading account: %w", err)
	}
	var chainID string
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE asset_id = $1 AND kind = 'chain'`, assetID).Scan(&chainID); err != nil {
		return SettleResult{}, fmt.Errorf("reading chain account: %w", err)
	}

	var txID string
	sum := sha256.Sum256([]byte("settle#" + key))
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (source_type, idempotency_key, request_hash)
		VALUES ('withdrawal_settle', $1, $2) RETURNING id`, key, hex.EncodeToString(sum[:])).Scan(&txID); err != nil {
		return SettleResult{}, fmt.Errorf("inserting settle transaction: %w", err)
	}

	// Chain account first, then the user's: the same order Apply locks them in, so a
	// Settle and a concurrent deposit on the same asset can't deadlock.
	if _, err := tx.Exec(ctx, `UPDATE accounts SET total_balance = total_balance + $2::numeric WHERE id = $1`, chainID, amountText); err != nil {
		return SettleResult{}, fmt.Errorf("crediting chain account: %w", err)
	}
	// No balance re-check: this exact amount was reserved by this same Lock (invariant #7).
	if _, err := tx.Exec(ctx, `
		UPDATE accounts SET total_balance = total_balance - $2::numeric, locked_balance = locked_balance - $2::numeric
		WHERE id = $1`, accountID, amountText); err != nil {
		return SettleResult{}, fmt.Errorf("debiting user account: %w", err)
	}
	for _, e := range []struct{ account, amount string }{{accountID, "-" + amountText}, {chainID, amountText}} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ($1, $2, $3::numeric)`, txID, e.account, e.amount); err != nil {
			return SettleResult{}, fmt.Errorf("inserting entry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SettleResult{}, fmt.Errorf("committing tx: %w", err)
	}
	return SettleResult{IdempotencyKey: key, LedgerTransactionID: txID}, nil
}

// Release gives a Lock's capacity back: the withdrawal never happened. It posts no
// Entry, total_balance was never touched, so there is nothing to compensate
// (invariant #8). Releasing an already-released lock is a no-op; releasing a settled
// one is ErrLockAlreadySettled, because money really moved.
func (s *Store) Release(ctx context.Context, key string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var accountID, amountText string
	err = tx.QueryRow(ctx, `
		UPDATE locks SET status = 'released', resolved_at = now()
		WHERE idempotency_key = $1 AND status = 'active'
		RETURNING account_id, amount::text`, key).Scan(&accountID, &amountText)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		_, err := s.resolvedLock(ctx, key, LockReleased)
		return err
	}
	if err != nil {
		return fmt.Errorf("resolving lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET locked_balance = locked_balance - $2::numeric WHERE id = $1`, accountID, amountText); err != nil {
		return fmt.Errorf("releasing balance: %w", err)
	}
	return tx.Commit(ctx)
}

// resolvedLock handles a Settle/Release that found no active lock: it was already
// resolved (same way = idempotent no-op, other way = conflict) or never existed.
func (s *Store) resolvedLock(ctx context.Context, key string, want LockStatus) (SettleResult, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM locks WHERE idempotency_key = $1`, key).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return SettleResult{}, ErrLockNotFound
	}
	if err != nil {
		return SettleResult{}, fmt.Errorf("reading lock: %w", err)
	}
	if LockStatus(status) != want {
		if want == LockSettled {
			return SettleResult{}, ErrLockAlreadyReleased
		}
		return SettleResult{}, ErrLockAlreadySettled
	}
	res := SettleResult{IdempotencyKey: key}
	if want == LockSettled {
		if err := s.pool.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE idempotency_key = $1`, key).Scan(&res.LedgerTransactionID); err != nil {
			return SettleResult{}, fmt.Errorf("reading settle transaction: %w", err)
		}
	}
	return res, nil
}
