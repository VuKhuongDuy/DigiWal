package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ConfirmedSince returns the chain's `confirmed` requests mined at or after minBlock: the ones a
// reorg could still take back.
func (s *Store) ConfirmedSince(ctx context.Context, chainID, minBlock int64) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM withdrawal_requests
		WHERE status = 'confirmed' AND chain_id = $1 AND confirmed_block >= $2
		ORDER BY nonce`, chainID, minBlock)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Withdrawal, error) { return scan(r) })
}

// OpenNonceGap records a Nonce Gap at fromNonce and, in the same transaction, moves every request
// on that chain at nonce >= fromNonce that holds a tx (signed, broadcast or confirmed) to
// `disputed` (invariant #8: by EVM nonce ordering none of them can be canonically confirmed while
// N is missing). Idempotent. Returns the ids it disputed now.
func (s *Store) OpenNonceGap(ctx context.Context, chainID int64, fromNonce int64, txHash string) ([]string, error) {
	var ids []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO nonce_gaps (chain_id, from_nonce, tx_hash) VALUES ($1, $2, $3)
			ON CONFLICT (chain_id, from_nonce) DO NOTHING`, chainID, fromNonce, txHash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			UPDATE withdrawal_requests
			SET disputed_from = status, status = 'disputed', disputed_at = now(), updated_at = now()
			WHERE chain_id = $1 AND nonce >= $2 AND status IN ('signed', 'broadcast', 'confirmed')
			RETURNING withdrawal_id`, chainID, fromNonce)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return ids, err
}

// NonceGap is one recorded gap.
type NonceGap struct {
	ChainID   int64
	FromNonce int64
}

// UnreportedNonceGaps are gaps wallet-service has not acknowledged yet.
func (s *Store) UnreportedNonceGaps(ctx context.Context) ([]NonceGap, error) {
	rows, err := s.pool.Query(ctx, `SELECT chain_id, from_nonce FROM nonce_gaps WHERE reported_at IS NULL ORDER BY detected_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NonceGap, error) {
		var g NonceGap
		err := r.Scan(&g.ChainID, &g.FromNonce)
		return g, err
	})
}

func (s *Store) MarkNonceGapReported(ctx context.Context, g NonceGap) error {
	_, err := s.pool.Exec(ctx, `UPDATE nonce_gaps SET reported_at = now() WHERE chain_id = $1 AND from_nonce = $2 AND reported_at IS NULL`,
		g.ChainID, g.FromNonce)
	return err
}
