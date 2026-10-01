// Package store holds withdrawal-service's Withdrawal Requests (docs/design/withdrawal-flow.md).
package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"digiwal/services/withdrawal-service/migrations"
)

// Status values of the Withdrawal Request state machine.
const (
	StatusRequested = "requested"
	StatusValidated = "validated"
	StatusReserved  = "reserved"
	StatusSigned    = "signed"
	StatusBroadcast = "broadcast"
	StatusConfirmed = "confirmed"
	StatusRejected  = "rejected"
	StatusFailed    = "failed"
	StatusDisputed  = "disputed"
)

// ErrNotFound: no request with that (user_id, idempotency_key) — including one that exists
// for another user (invariant #20: indistinguishable from a key that does not exist).
var ErrNotFound = errors.New("withdrawal request not found")

// NewRequest is what a client asks for. UserID is the authenticated user, injected by
// api-gateway, never client-chosen.
type NewRequest struct {
	UserID         string
	IdempotencyKey string
	AssetID        string
	Amount         *big.Int
	ToAddress      string
}

type Withdrawal struct {
	ID             string // withdrawal_id, server-minted
	UserID         string
	IdempotencyKey string
	AssetID        string
	Amount         *big.Int
	ToAddress      string
	Status         string
	RejectReason   *string
	TxHash         *string
	CreatedAt      time.Time
	UpdatedAt      time.Time

	// Filled in by the pipeline (task 3.3).
	Nonce            *int64
	SignedRawTx      *string
	ChainID          *int64
	BroadcastAt      *time.Time
	ConfirmedBlock   *int64
	SettleLedgerTxID *string
	ConfirmedAt      *time.Time

	// Set when a Nonce Gap disputed the request (task 3.4): the status it was in.
	DisputedFrom *string
	DisputedAt   *time.Time

	// The chain head the Reaper first saw this request `broadcast` at (task 3.9).
	BroadcastHeadSeen *int64

	// The chain head the latest version of the tx was last sent at; nil = not sent yet (task 3.13).
	SentHead *int64
}

// Key is the Withdrawal Key every downstream call uses (ledger Lock/Settle/Release,
// wallet-service signing): globally unique, recomputable from the row (invariant #20).
func (w Withdrawal) Key() string { return "withdrawal:" + w.ID }

// SameRequest reports whether r asks for exactly what w already records.
func (w Withdrawal) SameRequest(r NewRequest) bool {
	return w.AssetID == r.AssetID && w.Amount.Cmp(r.Amount) == 0 && w.ToAddress == r.ToAddress
}

type Store struct{ pool *pgxpool.Pool }

// Connect opens the pool and applies any pending migrations.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	if err := MigrateUp(dsn); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// MigrateUp applies every pending embedded migration; a no-op when up to date.
func MigrateUp(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

const columns = `withdrawal_id, user_id, idempotency_key, asset_id, amount::text, to_address,
	status, reject_reason, tx_hash, created_at, updated_at,
	nonce, signed_raw_tx, chain_id, broadcast_at, confirmed_block, settle_ledger_tx_id, confirmed_at,
	disputed_from, disputed_at, broadcast_head_seen, sent_head`

// Create is invariant #11's first step: insert the row, or — if (user_id, idempotency_key)
// already exists, however concurrently — read back the existing one. created reports which.
// Nothing else (validation, Reservation, signing) may run before this returns.
func (s *Store) Create(ctx context.Context, r NewRequest) (w Withdrawal, created bool, err error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO withdrawal_requests (user_id, idempotency_key, asset_id, amount, to_address)
		VALUES ($1, $2, $3, $4::numeric, $5)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING `+columns,
		r.UserID, r.IdempotencyKey, r.AssetID, r.Amount.String(), r.ToAddress)
	w, err = scan(row)
	switch {
	case err == nil:
		return w, true, nil
	case errors.Is(err, ErrNotFound): // lost the race, or a plain duplicate
		w, err = s.Get(ctx, r.UserID, r.IdempotencyKey)
		return w, false, err
	default:
		return Withdrawal{}, false, err
	}
}

// ErrStateChanged: a guarded transition found the row no longer in the expected status
// (e.g. the Reaper failed it first). The caller re-reads the row instead of overwriting it.
var ErrStateChanged = errors.New("withdrawal request is no longer in the expected status")

// Transition moves the row from `from` to `to` (and records reason, for `rejected`) in one
// guarded UPDATE, so two writers can never both succeed (invariant #11).
func (s *Store) Transition(ctx context.Context, id, from, to string, reason *string) (Withdrawal, error) {
	return guarded(scan(s.pool.QueryRow(ctx, `
		UPDATE withdrawal_requests SET status = $3, reject_reason = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = $2
		RETURNING `+columns, id, from, to, reason)))
}

// GetByID reads a request by its withdrawal_id (operator tooling; never exposed to clients).
func (s *Store) GetByID(ctx context.Context, id string) (Withdrawal, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM withdrawal_requests WHERE withdrawal_id = $1`, id))
}

// Get returns the user's own request for that key (invariant #20: never another user's).
func (s *Store) Get(ctx context.Context, userID, idempotencyKey string) (Withdrawal, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM withdrawal_requests WHERE user_id = $1 AND idempotency_key = $2`,
		userID, idempotencyKey))
}

func scan(row pgx.Row) (Withdrawal, error) {
	var w Withdrawal
	var amount string
	err := row.Scan(&w.ID, &w.UserID, &w.IdempotencyKey, &w.AssetID, &amount, &w.ToAddress,
		&w.Status, &w.RejectReason, &w.TxHash, &w.CreatedAt, &w.UpdatedAt,
		&w.Nonce, &w.SignedRawTx, &w.ChainID, &w.BroadcastAt, &w.ConfirmedBlock, &w.SettleLedgerTxID, &w.ConfirmedAt,
		&w.DisputedFrom, &w.DisputedAt, &w.BroadcastHeadSeen, &w.SentHead)
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, err
	}
	var ok bool
	if w.Amount, ok = new(big.Int).SetString(amount, 10); !ok {
		return Withdrawal{}, fmt.Errorf("unparseable amount %q", amount)
	}
	return w, nil
}
