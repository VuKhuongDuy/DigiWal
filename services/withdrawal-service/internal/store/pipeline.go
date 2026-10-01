package store

import (
	"context"
	"errors"
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
	return guarded(scan(s.pool.QueryRow(ctx, `
		UPDATE withdrawal_requests
		SET status = 'signed', chain_id = $2, nonce = $3, signed_raw_tx = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'reserved'
		RETURNING `+columns, id, chainID, int64(nonce), rawTx)))
}

// MarkBroadcast moves signed → broadcast with the hash of the version sent and the chain head it
// was sent at, and writes the withdrawals.broadcast event to the outbox in the same transaction
// (payload built by the caller from the result).
func (s *Store) MarkBroadcast(ctx context.Context, id, txHash string, at time.Time, head int64, event func(Withdrawal) (topic string, payload []byte, err error)) (Withdrawal, error) {
	return s.transitionWithEvent(ctx, event, `
		UPDATE withdrawal_requests
		SET status = 'broadcast', tx_hash = $2, broadcast_at = $3, sent_head = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'signed'
		RETURNING `+columns, id, txHash, at, head)
}

// MarkConfirmed moves broadcast → confirmed after the Settle, recording the hash that was mined
// (any version of the tx — it can differ from the one broadcast first), the block and the ledger
// transaction, and writes withdrawals.confirmed to the outbox in the same transaction.
func (s *Store) MarkConfirmed(ctx context.Context, id, txHash string, block int64, ledgerTxID string, at time.Time, event func(Withdrawal) (string, []byte, error)) (Withdrawal, error) {
	return s.transitionWithEvent(ctx, event, `
		UPDATE withdrawal_requests
		SET status = 'confirmed', tx_hash = $5, confirmed_block = $2, settle_ledger_tx_id = $3, confirmed_at = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'broadcast'
		RETURNING `+columns, id, block, ledgerTxID, at, txHash)
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

func (s *Store) transitionWithEvent(ctx context.Context, event func(Withdrawal) (string, []byte, error), sql string, args ...any) (Withdrawal, error) {
	var out Withdrawal
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		w, err := guarded(scan(tx.QueryRow(ctx, sql, args...)))
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
