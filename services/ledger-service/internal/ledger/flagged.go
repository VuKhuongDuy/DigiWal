package ledger

import (
	"context"
	"fmt"
	"math/big"
	"strings"
)

// FlaggedDeposit is a confirmed deposit whose sender is on the Screening List. It
// never becomes a Ledger Transaction automatically (compliance-screening.md #1).
type FlaggedDeposit struct {
	ChainID     int64
	TxHash      string
	LogIndex    int64
	AssetID     string
	FromAddress string
	ToAddress   string
	Amount      *big.Int
	BlockNumber int64
}

// InsertFlaggedDeposit records a Flagged Deposit as pending. Idempotent on the
// natural key (invariant #4): a Kafka redelivery can't create a second flag.
// Reports whether a new row was created.
func (s *Store) InsertFlaggedDeposit(ctx context.Context, d FlaggedDeposit) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO flagged_deposits (chain_id, tx_hash, log_index, asset_id, from_address, to_address, amount, block_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8)
		ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
		d.ChainID, strings.ToLower(d.TxHash), d.LogIndex, strings.ToLower(d.AssetID),
		strings.ToLower(d.FromAddress), strings.ToLower(d.ToAddress), d.Amount.String(), d.BlockNumber)
	if err != nil {
		return false, fmt.Errorf("inserting flagged deposit: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordFailedEvent keeps an event that can never be applied (malformed, unknown
// asset or address) so it is not lost silently. There is no dead-letter design yet.
func (s *Store) RecordFailedEvent(ctx context.Context, topic, key string, payload []byte, reason string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO failed_events (topic, message_key, payload, error) VALUES ($1, $2, $3, $4)`,
		topic, key, payload, reason); err != nil {
		return fmt.Errorf("recording failed event: %w", err)
	}
	return nil
}

// CountFlaggedDeposits returns how many Flagged Deposits are still pending review.
func (s *Store) CountFlaggedDeposits(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM flagged_deposits WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// CountFailedEvents returns how many unprocessable events are parked.
func (s *Store) CountFailedEvents(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM failed_events`).Scan(&n)
	return n, err
}
