package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type SourceType string

const (
	SourceDeposit    SourceType = "deposit"
	SourceWithdrawal SourceType = "withdrawal"
	SourceCorrection SourceType = "correction"
)

type AccountRef string

const (
	RefUser  AccountRef = "user"
	RefChain AccountRef = "chain"
)

// Entry is one signed posting. Positive credits the account, negative debits it.
type Entry struct {
	Ref     AccountRef
	UserID  string // required for RefUser, forbidden for RefChain
	AssetID string
	Amount  *big.Int
}

// DepositRef is a deposit's natural key (chain_id, tx_hash, log_index); log_index
// is -1 for native coin. Stored alongside the idempotency key.
type DepositRef struct {
	ChainID  int64
	TxHash   string
	LogIndex int64
}

// Transaction is a Ledger Transaction to apply.
type Transaction struct {
	IdempotencyKey string
	Source         SourceType
	Entries        []Entry
	Deposit        *DepositRef // deposits only
}

type Status string

const (
	Applied        Status = "applied"
	AlreadyApplied Status = "already_applied"
)

var (
	ErrInvalid              = errors.New("invalid ledger transaction")
	ErrUnbalanced           = errors.New("entries do not sum to zero")
	ErrInsufficientFunds    = errors.New("insufficient funds")
	ErrUnknownAsset         = errors.New("asset is not in the registry")
	ErrIdempotencyKeyReused = errors.New("idempotency key already used for a different transaction")
)

// Validate checks everything that can be checked without a database. Invariant #2
// (entries sum to zero) lives here so it is unit-testable in isolation: Postgres
// has no simple constraint for "these rows I'm about to insert sum to zero".
//
// The sum must be zero *per asset*: a +1 ETH entry can't be balanced by a -1 of a
// different token, even though the raw numbers cancel.
func (t Transaction) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if t.IdempotencyKey == "" || len(t.IdempotencyKey) > 256 {
		return bad("idempotency_key is required (max 256 chars)")
	}
	switch t.Source {
	case SourceDeposit, SourceWithdrawal, SourceCorrection:
	default:
		return bad("unknown source_type %q", t.Source)
	}
	if (t.Source == SourceDeposit) != (t.Deposit != nil) {
		return bad("a deposit carries its (chain_id, tx_hash, log_index), and only a deposit does")
	}
	if len(t.Entries) < 2 {
		return bad("a ledger transaction needs at least 2 entries")
	}
	sums := map[string]*big.Int{}
	for i, e := range t.Entries {
		if e.Amount == nil || e.Amount.Sign() == 0 {
			return bad("entry %d: amount must be non-zero", i)
		}
		if new(big.Int).Abs(e.Amount).Cmp(maxAmount) > 0 {
			return bad("entry %d: amount exceeds uint256", i)
		}
		if !uuidRe.MatchString(strings.ToLower(e.AssetID)) {
			return bad("entry %d: asset_id is not a uuid", i)
		}
		switch e.Ref {
		case RefUser:
			if e.UserID == "" {
				return bad("entry %d: a user entry needs user_id", i)
			}
		case RefChain:
			if e.UserID != "" {
				return bad("entry %d: a chain entry must not carry user_id", i)
			}
		default:
			return bad("entry %d: account_ref must be user or chain", i)
		}
		k := strings.ToLower(e.AssetID)
		if sums[k] == nil {
			sums[k] = new(big.Int)
		}
		sums[k].Add(sums[k], e.Amount)
	}
	for asset, sum := range sums {
		if sum.Sign() != 0 {
			return fmt.Errorf("%w: asset %s is off by %s", ErrUnbalanced, asset, sum)
		}
	}
	return nil
}

// hash binds an idempotency key to the exact content it was first used for, so a
// replay is only a no-op when it really is the same transaction.
func (t Transaction) hash() string {
	es := make([]string, len(t.Entries))
	for i, e := range t.Entries {
		es[i] = fmt.Sprintf("%s|%s|%s|%s", e.Ref, e.UserID, strings.ToLower(e.AssetID), e.Amount)
	}
	sort.Strings(es)
	dep := ""
	if t.Deposit != nil {
		dep = fmt.Sprintf("%d|%s|%d", t.Deposit.ChainID, strings.ToLower(t.Deposit.TxHash), t.Deposit.LogIndex)
	}
	sum := sha256.Sum256([]byte(string(t.Source) + "#" + dep + "#" + strings.Join(es, ";")))
	return hex.EncodeToString(sum[:])
}

// Apply posts a Ledger Transaction atomically and idempotently (ledger.md invariants
// #1-#4), in one DB transaction: either every entry lands and every balance moves,
// or nothing does.
//
//   - Already applied (same key, same content): AlreadyApplied, nothing changes.
//   - Same key, different content: ErrIdempotencyKeyReused.
//   - A user account that would go negative: ErrInsufficientFunds, nothing written.
//     The row lock the UPDATE takes is what serialises concurrent debits (invariant #1).
func (s *Store) Apply(ctx context.Context, t Transaction) (Status, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var chainID, logIndex *int64
	var txHash *string
	if t.Deposit != nil {
		h := strings.ToLower(t.Deposit.TxHash)
		chainID, txHash, logIndex = &t.Deposit.ChainID, &h, &t.Deposit.LogIndex
	}
	hash := t.hash()

	var txID string
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (source_type, idempotency_key, request_hash, chain_id, tx_hash, log_index)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, string(t.Source), t.IdempotencyKey, hash, chainID, txHash, logIndex).Scan(&txID)
	switch {
	case errors.Is(err, pgx.ErrNoRows): // this key was applied before
		var prev string
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM ledger_transactions WHERE idempotency_key = $1`, t.IdempotencyKey).Scan(&prev); err != nil {
			return "", fmt.Errorf("reading prior transaction: %w", err)
		}
		if prev != hash {
			return "", ErrIdempotencyKeyReused
		}
		return AlreadyApplied, nil
	case isUniqueViolation(err): // same deposit natural key under a different idempotency key
		return "", ErrIdempotencyKeyReused
	case err != nil:
		return "", fmt.Errorf("inserting ledger transaction: %w", err)
	}

	// Net per account, processed in one global order so two concurrent transactions
	// touching the same accounts always lock them in the same order (no deadlocks).
	type target struct {
		ref     AccountRef
		userID  string
		assetID string
		net     *big.Int
		id      string
	}
	byKey := map[string]*target{}
	for _, e := range t.Entries {
		k := strings.Join([]string{strings.ToLower(e.AssetID), string(e.Ref), e.UserID}, "|")
		if byKey[k] == nil {
			byKey[k] = &target{ref: e.Ref, userID: e.UserID, assetID: strings.ToLower(e.AssetID), net: new(big.Int)}
		}
		byKey[k].net.Add(byKey[k].net, e.Amount)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	accountIDs := map[string]string{}
	for _, k := range keys {
		tg := byKey[k]
		id, err := resolveAccount(ctx, tx, tg.ref, tg.userID, tg.assetID)
		if err != nil {
			return "", err
		}
		tg.id = id
		accountIDs[k] = id

		tag, err := tx.Exec(ctx, `
			UPDATE accounts SET balance = balance + $2::numeric
			WHERE id = $1 AND (kind = 'chain' OR balance + $2::numeric >= 0)`, id, tg.net.String())
		if err != nil {
			return "", fmt.Errorf("updating balance: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "", ErrInsufficientFunds
		}
	}

	for _, e := range t.Entries {
		k := strings.Join([]string{strings.ToLower(e.AssetID), string(e.Ref), e.UserID}, "|")
		if _, err := tx.Exec(ctx,
			`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ($1, $2, $3::numeric)`,
			txID, accountIDs[k], e.Amount.String()); err != nil {
			return "", fmt.Errorf("inserting entry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing tx: %w", err)
	}
	return Applied, nil
}

// resolveAccount returns the account id, creating a user's account on first use.
// The Chain Account is created with its asset and is never created here, and an
// asset that isn't active in the registry is refused (invariant #5).
func resolveAccount(ctx context.Context, tx pgx.Tx, ref AccountRef, userID, assetID string) (string, error) {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT active FROM assets WHERE id = $1`, assetID).Scan(&active); errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return "", ErrUnknownAsset
	} else if err != nil {
		return "", fmt.Errorf("reading asset: %w", err)
	}
	var id string
	var err error
	if ref == RefChain {
		err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE asset_id = $1 AND kind = 'chain'`, assetID).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO accounts (asset_id, kind, user_id) VALUES ($1, 'user', $2)
			ON CONFLICT (asset_id, user_id) WHERE kind = 'user' DO UPDATE SET user_id = EXCLUDED.user_id
			RETURNING id`, assetID, userID).Scan(&id)
	}
	if err != nil {
		return "", fmt.Errorf("resolving account: %w", err)
	}
	return id, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Balance returns a user's Available Balance for an asset (0 if they have no account yet).
func (s *Store) Balance(ctx context.Context, userID, assetID string) (*big.Int, error) {
	var text string
	err := s.pool.QueryRow(ctx,
		`SELECT balance::text FROM accounts WHERE kind = 'user' AND user_id = $1 AND asset_id = $2`, userID, strings.ToLower(assetID)).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return new(big.Int), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading balance: %w", err)
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n, nil
}

// ChainBalance returns the asset's Chain Account balance: -(net funds ever moved
// between users and the chain), the figure to reconcile against on-chain custody.
func (s *Store) ChainBalance(ctx context.Context, assetID string) (*big.Int, error) {
	var text string
	if err := s.pool.QueryRow(ctx,
		`SELECT balance::text FROM accounts WHERE kind = 'chain' AND asset_id = $1`, strings.ToLower(assetID)).Scan(&text); err != nil {
		return nil, fmt.Errorf("reading chain balance: %w", err)
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n, nil
}
