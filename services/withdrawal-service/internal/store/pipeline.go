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

// MarkBroadcast moves signed → broadcast with the tx hash, and writes the withdrawals.broadcast
// event to the outbox in the same transaction (payload built by the caller from the result).
func (s *Store) MarkBroadcast(ctx context.Context, id, txHash string, at time.Time, event func(Withdrawal) (topic string, payload []byte, err error)) (Withdrawal, error) {
	return s.transitionWithEvent(ctx, event, `
		UPDATE withdrawal_requests
		SET status = 'broadcast', tx_hash = $2, broadcast_at = $3, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'signed'
		RETURNING `+columns, id, txHash, at)
}

// MarkConfirmed moves broadcast → confirmed after the Settle, recording the block and the
// ledger transaction, and writes withdrawals.confirmed to the outbox in the same transaction.
func (s *Store) MarkConfirmed(ctx context.Context, id string, block int64, ledgerTxID string, at time.Time, event func(Withdrawal) (string, []byte, error)) (Withdrawal, error) {
	return s.transitionWithEvent(ctx, event, `
		UPDATE withdrawal_requests
		SET status = 'confirmed', confirmed_block = $2, settle_ledger_tx_id = $3, confirmed_at = $4, updated_at = now()
		WHERE withdrawal_id = $1 AND status = 'broadcast'
		RETURNING `+columns, id, block, ledgerTxID, at)
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
