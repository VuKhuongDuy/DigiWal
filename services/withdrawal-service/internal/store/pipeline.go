package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// ListByStatus returns up to limit requests in status, oldest first: the pipeline's work queue.
func (s *Store) ListByStatus(ctx context.Context, status string, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM withdrawal_requests
		WHERE status = $1 ORDER BY updated_at, withdrawal_id LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Withdrawal, error) { return scan(r) })
}

// MarkSigned records wallet-service's signature and moves reserved → signed. The raw tx is kept
// so that every later broadcast, including after a crash, sends exactly this transaction.
func (s *Store) MarkSigned(ctx context.Context, id string, chainID int64, nonce uint64, rawTx string) (Withdrawal, error) {
	return s.timed(s.pool.QueryRow(ctx, `
		UPDATE withdrawal_requests
		SET status = 'signed', chain_id = $2, nonce = $3, signed_raw_tx = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'reserved'
		RETURNING `+columns+prevAge, id, chainID, int64(nonce), rawTx), StatusReserved, StatusSigned)
}

// MarkBroadcast moves signed → broadcast with the hash of the version sent and the chain head it
// was sent at, and writes the withdrawals.broadcast event to the outbox in the same transaction
// (payload built by the caller from the result).
func (s *Store) MarkBroadcast(ctx context.Context, id, txHash string, at time.Time, head int64, event func(Withdrawal) (topic string, payload []byte, err error)) (Withdrawal, error) {
	return s.transitionWithEvent(ctx, event, StatusSigned, StatusBroadcast, `
		UPDATE withdrawal_requests
		SET status = 'broadcast', tx_hash = $2, broadcast_at = $3, sent_head = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'signed'
		RETURNING `+columns+prevAge, id, txHash, at, head)
}

// MarkConfirmed moves broadcast → confirmed after the Settle, recording the hash that was mined
// (any version of the tx — it can differ from the one broadcast first), the block and the ledger
// transaction, and writes withdrawals.confirmed to the outbox in the same transaction.
//
// gas, when not nil, is written in the same transaction (task 3.15): the numbers the ledger's Gas Fee
// is computed from, posted afterwards by the pipeline.
func (s *Store) MarkConfirmed(ctx context.Context, id, txHash string, block int64, ledgerTxID string, at time.Time, gas *GasRecord, event func(Withdrawal) (string, []byte, error)) (Withdrawal, error) {
	g := gas.args()
	return s.transitionWithEvent(ctx, event, StatusBroadcast, StatusConfirmed, `
		UPDATE withdrawal_requests
		SET status = 'confirmed', tx_hash = $5, confirmed_block = $2, settle_ledger_tx_id = $3, confirmed_at = $4, updated_at = now(),
		    gas_tx_hash = $6, gas_used = $7, gas_price = $8::numeric, gas_purpose = $9
		WHERE withdrawal_id = $1 AND status = 'broadcast'
		RETURNING `+columns+prevAge, id, block, ledgerTxID, at, txHash, g[0], g[1], g[2], g[3])
}

// GasRecord is the gas a mined tx of ours spent, as its receipt reports it, waiting to be posted to
// the ledger as a Gas Fee (ledger.md invariant #12). Purpose is withdrawal, withdrawal_reverted or
// nonce_filler.
type GasRecord struct {
	TxHash   string
	GasUsed  uint64
	GasPrice *big.Int // wei
	Purpose  string
}

// args are the four SQL parameters (all NULL for no record).
func (g *GasRecord) args() [4]any {
	if g == nil {
		return [4]any{nil, nil, nil, nil}
	}
	return [4]any{g.TxHash, int64(g.GasUsed), g.GasPrice.String(), g.Purpose}
}

// GasPending is a GasRecord still to post.
type GasPending struct {
	ID      string
	ChainID int64
	GasRecord
}

// ListGasPending returns up to limit final requests whose gas has not been posted yet, oldest first.
func (s *Store) ListGasPending(ctx context.Context, limit int) ([]GasPending, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT withdrawal_id, coalesce(chain_id, 0), gas_tx_hash, gas_used, gas_price::text, gas_purpose
		FROM withdrawal_requests WHERE gas_purpose IS NOT NULL AND gas_posted_at IS NULL
		ORDER BY updated_at, withdrawal_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GasPending, error) {
		var g GasPending
		var used int64
		var price string
		if err := r.Scan(&g.ID, &g.ChainID, &g.TxHash, &used, &price, &g.Purpose); err != nil {
			return g, err
		}
		g.GasUsed = uint64(used)
		var ok bool
		if g.GasPrice, ok = new(big.Int).SetString(price, 10); !ok {
			return g, fmt.Errorf("unparseable gas price %q", price)
		}
		return g, nil
	})
}

// MarkGasPosted records that ledger-service accepted the Gas Fee. Idempotent.
func (s *Store) MarkGasPosted(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET gas_posted_at = now()
		WHERE withdrawal_id = $1 AND gas_posted_at IS NULL`, id)
	return err
}

// Tx is one Fee Bump version of a request's tx (replacement_seq >= 1).
type Tx struct {
	Seq  int
	Hash string
	Raw  string
}

// ListTxs returns the request's Fee Bump versions, oldest first. The original tx is
// withdrawal_requests.signed_raw_tx, not a row here.
func (s *Store) ListTxs(ctx context.Context, id string) ([]Tx, error) {
	rows, err := s.pool.Query(ctx, `SELECT replacement_seq, tx_hash, raw_tx FROM withdrawal_txs
		WHERE withdrawal_id = $1 ORDER BY replacement_seq`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tx, error) {
		var t Tx
		err := r.Scan(&t.Seq, &t.Hash, &t.Raw)
		return t, err
	})
}

// AddReplacement records a Fee Bump version and marks the latest version "not sent yet"
// (sent_head NULL), in one transaction. Only a `signed` or `broadcast` request takes one
// (ErrStateChanged otherwise). Idempotent on (id, seq): a replay changes nothing, so a crash
// between wallet-service's answer and this write is repaired by asking again.
func (s *Store) AddReplacement(ctx context.Context, id string, seq int, hash, raw string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM withdrawal_requests WHERE withdrawal_id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != StatusSigned && status != StatusBroadcast {
			return ErrStateChanged
		}
		tag, err := tx.Exec(ctx, `INSERT INTO withdrawal_txs (withdrawal_id, replacement_seq, tx_hash, raw_tx)
			VALUES ($1, $2, $3, $4) ON CONFLICT (withdrawal_id, replacement_seq) DO NOTHING`, id, seq, hash, raw)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET sent_head = NULL WHERE withdrawal_id = $1`, id)
		return err
	})
}

// MarkSent records that the latest version of a `broadcast` request was sent at head.
func (s *Store) MarkSent(ctx context.Context, id string, head int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET sent_head = $2
		WHERE withdrawal_id = $1 AND status = 'broadcast'`, id, head)
	return err
}

func (s *Store) transitionWithEvent(ctx context.Context, event func(Withdrawal) (string, []byte, error), from, to, sql string, args ...any) (Withdrawal, error) {
	var out Withdrawal
	var secs float64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		w, err := guarded(scan(timedRow{tx.QueryRow(ctx, sql, args...), &secs}))
		if err != nil {
			return err
		}
		topic, payload, err := event(w)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_outbox (withdrawal_id, topic, payload) VALUES ($1, $2, $3)
			ON CONFLICT (withdrawal_id, topic) DO NOTHING`, w.ID, topic, payload); err != nil {
			return err
		}
		out = w
		return nil
	})
	if err == nil {
		s.tell(from, to, secs) // after the commit, so a rolled-back transition is never reported
	}
	return out, err
}

func guarded(w Withdrawal, err error) (Withdrawal, error) {
	if errors.Is(err, ErrNotFound) {
		return Withdrawal{}, ErrStateChanged
	}
	return w, err
}

// OutboxEvent is one recorded-but-maybe-unpublished event.
type OutboxEvent struct {
	ID           int64
	WithdrawalID string
	Topic        string
	Payload      []byte
}

// UnpublishedEvents returns up to limit events Kafka has not acknowledged yet, in write order.
func (s *Store) UnpublishedEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, withdrawal_id, topic, payload FROM withdrawal_outbox
		WHERE published_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutboxEvent, error) {
		var e OutboxEvent
		err := r.Scan(&e.ID, &e.WithdrawalID, &e.Topic, &e.Payload)
		return e, err
	})
}

// MarkPublished is called only after Kafka acknowledged the event.
func (s *Store) MarkPublished(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE withdrawal_outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL`, id)
	return err
}

// ListOlderThan returns up to limit requests in status that have not changed since before,
// oldest first: the Reaper's work queue (updated_at is when the row entered its status).
func (s *Store) ListOlderThan(ctx context.Context, status string, before time.Time, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM withdrawal_requests
		WHERE status = $1 AND updated_at < $2 ORDER BY updated_at, withdrawal_id LIMIT $3`, status, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Withdrawal, error) { return scan(r) })
}

// StampBroadcastHead records the chain head the Reaper first saw a request `broadcast` at. Guarded:
// only a still-`broadcast` row that has no stamp yet is touched, and updated_at is left alone.
func (s *Store) StampBroadcastHead(ctx context.Context, id string, head int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE withdrawal_requests SET broadcast_head_seen = $2
		WHERE withdrawal_id = $1 AND status = 'broadcast' AND broadcast_head_seen IS NULL`, id, head)
	return err
}

// MarkFailed moves a request from `from` to `failed` (guarded UPDATE), recording why, and in the
// same transaction writes the withdrawals.failed event to the outbox (invariant #24) — and, when
// blockAddress is not empty, blocks the request's user's Whitelist Entry for that EIP-55 address
// (invariant #25), and records gas (a mined Nonce Filler's, a reverted tx's) for posting (task 3.15). The outbox row is unique per (withdrawal, topic), so a request fails — and is
// reported — at most once. ErrStateChanged if the row is no longer in `from`.
func (s *Store) MarkFailed(ctx context.Context, id, from, reason, blockAddress string, gas *GasRecord, event func(Withdrawal) (topic string, payload []byte, err error)) (Withdrawal, error) {
	g := gas.args()
	var out Withdrawal
	var secs float64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		w, err := guarded(scan(timedRow{tx.QueryRow(ctx, `
			UPDATE withdrawal_requests SET status = 'failed', reject_reason = $3, updated_at = now(),
			    gas_tx_hash = $4, gas_used = $5, gas_price = $6::numeric, gas_purpose = $7
			WHERE withdrawal_id = $1 AND status = $2
			RETURNING `+columns+prevAge, id, from, reason, g[0], g[1], g[2], g[3]), &secs}))
		if err != nil {
			return err
		}
		topic, payload, err := event(w)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_outbox (withdrawal_id, topic, payload) VALUES ($1, $2, $3)
			ON CONFLICT (withdrawal_id, topic) DO NOTHING`, w.ID, topic, payload); err != nil {
			return err
		}
		if blockAddress != "" {
			if _, err := tx.Exec(ctx, `UPDATE whitelist_entries SET blocked_at = now()
				WHERE user_id = $1 AND address = $2 AND blocked_at IS NULL`, w.UserID, blockAddress); err != nil {
				return err
			}
		}
		out = w
		return nil
	})
	if err == nil {
		s.tell(from, StatusFailed, secs)
	}
	return out, err
}
