// Package store is chain-watcher's Postgres state: the watermark, the hashes of recently
// scanned blocks, and the Observed Transfers (deposit-flow.md).
package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"digiwal/services/chain-watcher/migrations"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDeepReorg means the node's chain diverged below a transfer we already confirmed.
// Confirmation Depth exists so this does not happen; if it does, deposit-flow.md Open
// Question #4 (out of scope) applies and a human must look — never handled automatically.
var ErrDeepReorg = errors.New("reorg deeper than Confirmation Depth")

// ErrLostWatermark means another writer moved the watermark under us (two instances).
var ErrLostWatermark = errors.New("watermark moved by another writer")

const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"

	// HashRetention is how many recent block hashes are kept for reorg detection. It has
	// to exceed Confirmation Depth by a margin, since a reorg is only recoverable while
	// its fork point is inside the window.
	HashRetention = 256
)

type Transfer struct {
	ChainID     int64
	TxHash      string // lower-case 0x hex
	LogIndex    int64  // -1 for native coin
	AssetID     string
	From, To    string // EIP-55 addresses
	UserID      string
	Amount      *big.Int
	BlockNumber int64
	BlockHash   string
	Status      string
	DetectedAt  time.Time
}

type BlockRef struct {
	Number int64
	Hash   string
}

type Store struct{ pool *pgxpool.Pool }

func Connect(ctx context.Context, dsn string) (*Store, error) { return ConnectWithPool(ctx, dsn, 0) }

// ConnectWithPool is Connect with an explicit pool size (0 keeps pgx's default of max(4, CPUs)). The size is a
// separate argument, not a pool_max_conns parameter in the DSN: the same DSN also goes to golang-migrate, which
// would send that parameter to the server as a runtime setting and be refused ("unrecognized configuration
// parameter"), so a service could not start.
func ConnectWithPool(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

// Pool exposes the pool for the advisory lock (single active instance).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func NewMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("loading embedded migrations: %w", err)
	}
	url := "pgx5://" + strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return nil, fmt.Errorf("creating migrator: %w", err)
	}
	return m, nil
}

func MigrateUp(dsn string) error {
	m, err := NewMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// Watermark returns the last fully scanned block; ok is false before the first scan.
func (s *Store) Watermark(ctx context.Context, chainID int64) (n int64, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT block_number FROM watermark WHERE chain_id = $1`, chainID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// StartAt sets the initial watermark. A no-op if one exists.
func (s *Store) StartAt(ctx context.Context, chainID, block int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO watermark (chain_id, block_number) VALUES ($1, $2) ON CONFLICT DO NOTHING`, chainID, block)
	return err
}

// ScannedHash is the stored hash of a scanned block; ok is false if it is not (or no
// longer) recorded.
func (s *Store) ScannedHash(ctx context.Context, chainID, n int64) (h string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT block_hash FROM scanned_blocks WHERE chain_id=$1 AND block_number=$2`, chainID, n).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return h, err == nil, err
}

// Rewind moves the watermark back to `to` after a reorg: it forgets the hashes above it and
// deletes the pending transfers above it (they were seen in blocks that are no longer
// canonical; if they are in the new chain the rescan records them again). A confirmed
// transfer above `to` is never touched — it returns ErrDeepReorg and changes nothing.
func (s *Store) Rewind(ctx context.Context, chainID, to int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var confirmed int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM observed_transfers WHERE chain_id=$1 AND block_number>$2 AND status='confirmed'`, chainID, to).Scan(&confirmed); err != nil {
		return err
	}
	if confirmed > 0 {
		return fmt.Errorf("%w: %d confirmed transfer(s) above block %d", ErrDeepReorg, confirmed, to)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM observed_transfers WHERE chain_id=$1 AND block_number>$2 AND status='pending'`, chainID, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM scanned_blocks WHERE chain_id=$1 AND block_number>$2`, chainID, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE watermark SET block_number=$2, updated_at=now() WHERE chain_id=$1`, chainID, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CommitRange records everything found in blocks (from-1, to] and advances the watermark
// from `from-1` to `to`, in one transaction (invariant #4: the watermark never gets ahead of
// what is durably recorded). Transfers already known are left alone (the natural key).
// If the watermark is not at from-1 any more, another instance moved it: nothing is written.
func (s *Store) CommitRange(ctx context.Context, chainID, from, to int64, blocks []BlockRef, transfers []Transfer) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE watermark SET block_number=$3, updated_at=now() WHERE chain_id=$1 AND block_number=$2`, chainID, from-1, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLostWatermark
	}
	for _, b := range blocks {
		if _, err := tx.Exec(ctx, `INSERT INTO scanned_blocks (chain_id, block_number, block_hash) VALUES ($1,$2,$3)
			ON CONFLICT (chain_id, block_number) DO UPDATE SET block_hash = EXCLUDED.block_hash`, chainID, b.Number, b.Hash); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM scanned_blocks WHERE chain_id=$1 AND block_number <= $2`, chainID, to-HashRetention); err != nil {
		return err
	}
	for _, t := range transfers {
		if _, err := tx.Exec(ctx, `INSERT INTO observed_transfers
			(chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount, block_number, block_hash, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9,$10,'pending')
			ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
			t.ChainID, t.TxHash, t.LogIndex, t.AssetID, t.From, t.To, t.UserID, t.Amount.String(), t.BlockNumber, t.BlockHash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

const transferCols = `chain_id, tx_hash, log_index, asset_id::text, from_address, to_address, user_id, amount::text, block_number, block_hash, status, detected_at`

func scanTransfers(rows pgx.Rows) ([]Transfer, error) {
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		var t Transfer
		var amount string
		if err := rows.Scan(&t.ChainID, &t.TxHash, &t.LogIndex, &t.AssetID, &t.From, &t.To, &t.UserID, &amount, &t.BlockNumber, &t.BlockHash, &t.Status, &t.DetectedAt); err != nil {
			return nil, err
		}
		t.Amount, _ = new(big.Int).SetString(amount, 10)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingUpTo lists pending transfers included at or below block n, oldest first.
func (s *Store) PendingUpTo(ctx context.Context, chainID, n int64) ([]Transfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+transferCols+` FROM observed_transfers
		WHERE chain_id=$1 AND status='pending' AND block_number <= $2 ORDER BY block_number, log_index LIMIT 500`, chainID, n)
	if err != nil {
		return nil, err
	}
	return scanTransfers(rows)
}

// Confirm moves a transfer pending -> confirmed. It reports whether this call did it: the
// guard is UPDATE ... WHERE status='pending' (invariant #3), so a transfer is confirmed once.
func (s *Store) Confirm(ctx context.Context, t Transfer) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE observed_transfers SET status='confirmed', confirmed_at=now()
		WHERE chain_id=$1 AND tx_hash=$2 AND log_index=$3 AND status='pending'`, t.ChainID, t.TxHash, t.LogIndex)
	return tag.RowsAffected() == 1, err
}

// UnpublishedDetected lists transfers whose deposits.detected has not been acknowledged by Kafka.
func (s *Store) UnpublishedDetected(ctx context.Context, chainID int64) ([]Transfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+transferCols+` FROM observed_transfers
		WHERE chain_id=$1 AND detected_published_at IS NULL ORDER BY block_number, log_index LIMIT 500`, chainID)
	if err != nil {
		return nil, err
	}
	return scanTransfers(rows)
}

func (s *Store) MarkDetectedPublished(ctx context.Context, t Transfer) error {
	_, err := s.pool.Exec(ctx, `UPDATE observed_transfers SET detected_published_at=now()
		WHERE chain_id=$1 AND tx_hash=$2 AND log_index=$3`, t.ChainID, t.TxHash, t.LogIndex)
	return err
}

// UnpublishedConfirmed lists confirmed transfers whose deposits.confirmed has not been acknowledged by Kafka.
func (s *Store) UnpublishedConfirmed(ctx context.Context, chainID int64) ([]Transfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+transferCols+` FROM observed_transfers
		WHERE chain_id=$1 AND status='confirmed' AND confirmed_published_at IS NULL ORDER BY block_number, log_index LIMIT 500`, chainID)
	if err != nil {
		return nil, err
	}
	return scanTransfers(rows)
}

func (s *Store) MarkConfirmedPublished(ctx context.Context, t Transfer) error {
	_, err := s.pool.Exec(ctx, `UPDATE observed_transfers SET confirmed_published_at=now()
		WHERE chain_id=$1 AND tx_hash=$2 AND log_index=$3 AND status='confirmed'`, t.ChainID, t.TxHash, t.LogIndex)
	return err
}

// Get returns one transfer (for tests and diagnostics).
func (s *Store) Get(ctx context.Context, chainID int64, txHash string, logIndex int64) (*Transfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+transferCols+` FROM observed_transfers WHERE chain_id=$1 AND tx_hash=$2 AND log_index=$3`, chainID, txHash, logIndex)
	if err != nil {
		return nil, err
	}
	ts, err := scanTransfers(rows)
	if err != nil || len(ts) == 0 {
		return nil, err
	}
	return &ts[0], nil
}
