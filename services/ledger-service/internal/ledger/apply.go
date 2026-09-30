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
	SourceCorrection SourceType = "correction"
	// SourceWithdrawalSettle is posted only by Settle (locks.go), never through Apply.
	SourceWithdrawalSettle SourceType = "withdrawal_settle"
)

type AccountRef string

const (
	RefUser  AccountRef = "user"
	RefChain AccountRef = "chain"
)

// Entry is one signed posting. Positive credits the account, negative debits it.
//
// For a correction only CorrectsEntryID is read: the account and the amount are
// derived server-side as the exact negation of the referenced entry, never taken
// from the caller (ledger.md invariant #11).
type Entry struct {
	Ref     AccountRef
	UserID  string // required for RefUser, forbidden for RefChain
	AssetID string
	Amount  *big.Int

	CorrectsEntryID string // corrections only
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
	ErrInvalidCorrection    = errors.New("invalid correction")
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
	case SourceDeposit, SourceCorrection: // Settle has its own operation (ledger.md); it never goes through Apply
	default:
		return bad("source_type must be deposit or correction, got %q", t.Source)
	}
	if (t.Source == SourceDeposit) != (t.Deposit != nil) {
		return bad("a deposit carries its (chain_id, tx_hash, log_index), and only a deposit does")
	}
	if len(t.Entries) < 2 {
		return bad("a ledger transaction needs at least 2 entries")
	}
	if t.Source == SourceCorrection {
		// Amounts and accounts come from the referenced entries, not the caller, so all
		// that can be checked here is that every entry names one (invariant #11). Whether
		// the derived reversal balances is checked once it has been derived.
		for i, e := range t.Entries {
			if !uuidRe.MatchString(strings.ToLower(e.CorrectsEntryID)) {
				return fmt.Errorf("%w: entry %d: corrects_entry_id is required and must be a uuid", ErrInvalidCorrection, i)
			}
		}
		return nil
	}
	sums := map[string]*big.Int{}
	for i, e := range t.Entries {
		if e.Amount == nil || e.Amount.Sign() == 0 {
			return bad("entry %d: amount must be non-zero", i)
		}
		if new(big.Int).Abs(e.Amount).Cmp(maxAmount) > 0 {
			return bad("entry %d: amount exceeds uint256", i)
		}
		if e.CorrectsEntryID != "" {
			return bad("entry %d: corrects_entry_id is only valid on a correction", i)
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
		if t.Source == SourceCorrection {
			es[i] = "corrects|" + strings.ToLower(e.CorrectsEntryID)
			continue
		}
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

	var posts []posting
	if t.Source == SourceCorrection {
		if posts, err = deriveCorrection(ctx, tx, t); err != nil {
			return "", err
		}
	} else {
		for _, e := range t.Entries {
			id, err := resolveAccount(ctx, tx, e.Ref, e.UserID, e.AssetID)
			if err != nil {
				return "", err
			}
			posts = append(posts, posting{accountID: id, order: orderKey(e.AssetID, string(e.Ref), e.UserID), amount: e.Amount})
		}
	}

	// Net per account, applied in one global order so two concurrent transactions
	// touching the same accounts always lock them in the same order (no deadlocks).
	// Settle takes its two rows in this same order (chain before user).
	net := map[string]*big.Int{}
	order := map[string]string{}
	for _, p := range posts {
		if net[p.accountID] == nil {
			net[p.accountID] = new(big.Int)
			order[p.accountID] = p.order
		}
		net[p.accountID].Add(net[p.accountID], p.amount)
	}
	ids := make([]string, 0, len(net))
	for id := range net {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return order[ids[i]] < order[ids[j]] })
	for _, id := range ids {
		// Invariant #9: a debit that is not a Settle is checked against Available
		// Balance (total - locked), so a correction can't undercut an active Lock.
		tag, err := tx.Exec(ctx, `
			UPDATE accounts SET total_balance = total_balance + $2::numeric
			WHERE id = $1 AND (kind = 'chain' OR total_balance - locked_balance + $2::numeric >= 0)`, id, net[id].String())
		if err != nil {
			return "", fmt.Errorf("updating balance: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "", ErrInsufficientFunds
		}
	}

	for _, p := range posts {
		var corrects *string
		if p.corrects != "" {
			corrects = &p.corrects
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO entries (ledger_transaction_id, account_id, amount, corrects_entry_id) VALUES ($1, $2, $3::numeric, $4)`,
			txID, p.accountID, p.amount.String(), corrects); err != nil {
			if isUniqueViolation(err) {
				return "", fmt.Errorf("%w: an entry can only be corrected once", ErrInvalidCorrection)
			}
			return "", fmt.Errorf("inserting entry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing tx: %w", err)
	}
	return Applied, nil
}

// posting is one derived entry, ready to insert.
type posting struct {
	accountID string
	order     string // global lock-order key: asset | kind | user
	amount    *big.Int
	corrects  string
}

func orderKey(assetID, kind, userID string) string {
	return strings.Join([]string{strings.ToLower(assetID), kind, userID}, "|")
}

// deriveCorrection builds a correction's postings as the exact negation of the entries
// it names — same account, opposite sign, same magnitude — ignoring any account or
// amount the caller sent (invariant #11). The reversal must balance per asset, so a
// caller can't reverse just one leg of a transaction.
func deriveCorrection(ctx context.Context, tx pgx.Tx, t Transaction) ([]posting, error) {
	var posts []posting
	seen := map[string]bool{}
	sums := map[string]*big.Int{}
	for _, e := range t.Entries {
		id := strings.ToLower(e.CorrectsEntryID)
		if seen[id] {
			return nil, fmt.Errorf("%w: entry %s named twice", ErrInvalidCorrection, id)
		}
		seen[id] = true
		var accountID, assetID, kind, amount string
		var userID *string
		err := tx.QueryRow(ctx, `
			SELECT e.account_id, a.asset_id, a.kind, a.user_id, e.amount::text
			FROM entries e JOIN accounts a ON a.id = e.account_id WHERE e.id = $1`, id).
			Scan(&accountID, &assetID, &kind, &userID, &amount)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: entry %s does not exist", ErrInvalidCorrection, id)
		}
		if err != nil {
			return nil, fmt.Errorf("reading entry to correct: %w", err)
		}
		orig, _ := new(big.Int).SetString(amount, 10)
		neg := new(big.Int).Neg(orig)
		u := ""
		if userID != nil {
			u = *userID
		}
		posts = append(posts, posting{accountID: accountID, order: orderKey(assetID, kind, u), amount: neg, corrects: id})
		if sums[assetID] == nil {
			sums[assetID] = new(big.Int)
		}
		sums[assetID].Add(sums[assetID], neg)
	}
	for asset, sum := range sums {
		if sum.Sign() != 0 {
			return nil, fmt.Errorf("%w: the reversal of asset %s is off by %s (reverse every leg of the transaction)", ErrInvalidCorrection, asset, sum)
		}
	}
	return posts, nil
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

// Balances is a user's balance for one asset (ledger.md): Total is what is settled,
// Locked is held for in-flight withdrawals, Available = Total - Locked is what can be withdrawn.
type Balances struct {
	Total, Locked, Available *big.Int
}

// BalancesFor returns the user's balances for an asset (all zero if they have no account yet).
func (s *Store) BalancesFor(ctx context.Context, userID, assetID string) (Balances, error) {
	var total, locked string
	err := s.pool.QueryRow(ctx,
		`SELECT total_balance::text, locked_balance::text FROM accounts WHERE kind = 'user' AND user_id = $1 AND asset_id = $2`,
		userID, strings.ToLower(assetID)).Scan(&total, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balances{new(big.Int), new(big.Int), new(big.Int)}, nil
	}
	if err != nil {
		return Balances{}, fmt.Errorf("reading balances: %w", err)
	}
	t, _ := new(big.Int).SetString(total, 10)
	l, _ := new(big.Int).SetString(locked, 10)
	return Balances{Total: t, Locked: l, Available: new(big.Int).Sub(t, l)}, nil
}

// Balance returns a user's Available Balance for an asset.
func (s *Store) Balance(ctx context.Context, userID, assetID string) (*big.Int, error) {
	b, err := s.BalancesFor(ctx, userID, assetID)
	if err != nil {
		return nil, err
	}
	return b.Available, nil
}

// ChainBalance returns the asset's Chain Account balance: -(net funds ever moved
// between users and the chain), the figure to reconcile against on-chain custody.
func (s *Store) ChainBalance(ctx context.Context, assetID string) (*big.Int, error) {
	var text string
	if err := s.pool.QueryRow(ctx,
		`SELECT total_balance::text FROM accounts WHERE kind = 'chain' AND asset_id = $1`, strings.ToLower(assetID)).Scan(&text); err != nil {
		return nil, fmt.Errorf("reading chain balance: %w", err)
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n, nil
}

// AssetBalance is one row of a user's balance sheet.
type AssetBalance struct {
	AssetID   string
	Symbol    string
	Total     *big.Int
	Locked    *big.Int
	Available *big.Int
}

// BalanceSheet returns one row per active Asset Registry entry, whether or not the user has
// an Account for it: a never-touched asset reads 0/0/0, never a missing row
// (integration-contracts.md §2). assetID, if not empty, restricts the sheet to that asset.
func (s *Store) BalanceSheet(ctx context.Context, userID, assetID string) ([]AssetBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, a.symbol, COALESCE(acc.total_balance, 0)::text, COALESCE(acc.locked_balance, 0)::text
		FROM assets a
		LEFT JOIN accounts acc ON acc.asset_id = a.id AND acc.kind = 'user' AND acc.user_id = $1
		WHERE a.active AND ($2 = '' OR a.id::text = $2)
		ORDER BY a.symbol, a.id`, userID, strings.ToLower(assetID))
	if err != nil {
		return nil, fmt.Errorf("reading balance sheet: %w", err)
	}
	defer rows.Close()
	out := []AssetBalance{}
	for rows.Next() {
		var b AssetBalance
		var total, locked string
		if err := rows.Scan(&b.AssetID, &b.Symbol, &total, &locked); err != nil {
			return nil, err
		}
		b.Total, _ = new(big.Int).SetString(total, 10)
		b.Locked, _ = new(big.Int).SetString(locked, 10)
		b.Available = new(big.Int).Sub(b.Total, b.Locked)
		out = append(out, b)
	}
	return out, rows.Err()
}
