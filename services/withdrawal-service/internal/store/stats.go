package store

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// AllStatuses is every status a request can have; each is exported even at zero so its series does not vanish.
var AllStatuses = []string{StatusRequested, StatusValidated, StatusReserved, StatusSigned, StatusBroadcast, StatusConfirmed, StatusRejected, StatusFailed, StatusDisputed}

// StuckThresholds are the Reaper's own timeouts (invariant 2 of monitoring-dashboard.md: one definition of
// "stuck", passed from main to both the Reaper and the metrics).
type StuckThresholds struct {
	Request, Reservation time.Duration
	BroadcastBlocks      int64
}

// Failure is a count of failed or rejected requests with a reason.
type Failure struct {
	Status, Reason string
	Count          int64
}

// Stats is what withdrawal-service exports as metrics from its own database (monitoring-dashboard.md section 5,
// reconciliation-audit.md decision (c)).
type Stats struct {
	Created   int64               // every request ever recorded
	ByStatus  map[string]int64    // current status counts, every status present
	Confirmed map[string]*big.Int // by asset id: amount of confirmed requests, minor units
	Failures  []Failure

	// For confirmed requests with both timestamps: how long our side took (created -> broadcast) and how long the
	// chain took (broadcast -> confirmed to depth), summed so that rate(sum)/rate(count) is the average.
	TimedCount     int64
	ProcessingSecs float64
	ChainWaitSecs  float64

	Stuck     map[string]int64   // requested, validated, reserved, signed (broadcast needs the chain head: StuckBroadcast)
	OldestAge map[string]float64 // seconds, non-terminal statuses, 0 when none

	NonceGaps, NonceGapsUnreported int64
	WhitelistBlocked               int64

	// The outbox: events written with a state change and not yet acknowledged by Kafka. A growing backlog or a rising
	// oldest age means the publisher (or Kafka) is behind, and history-service / ledger-service see events late.
	OutboxUnpublished int64
	OutboxOldestAge   float64 // seconds; 0 when nothing is waiting
}

var nonTerminal = []string{StatusRequested, StatusValidated, StatusReserved, StatusSigned, StatusBroadcast, StatusDisputed}

// Stats reads the figures in one REPEATABLE READ snapshot, using the database's own clock for ages.
func (s *Store) Stats(ctx context.Context, th StuckThresholds) (Stats, error) {
	st := Stats{ByStatus: map[string]int64{}, Confirmed: map[string]*big.Int{}, Stuck: map[string]int64{}, OldestAge: map[string]float64{}}
	for _, status := range AllStatuses {
		st.ByStatus[status] = 0
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	rows, err := tx.Query(ctx, `SELECT status, count(*) FROM withdrawal_requests GROUP BY status`)
	if err != nil {
		return st, fmt.Errorf("status counts: %w", err)
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.ByStatus[status] = n
		st.Created += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT asset_id, coalesce(sum(amount), 0)::text FROM withdrawal_requests WHERE status = 'confirmed' GROUP BY asset_id`)
	if err != nil {
		return st, fmt.Errorf("confirmed amounts: %w", err)
	}
	for rows.Next() {
		var asset, amt string
		if err := rows.Scan(&asset, &amt); err != nil {
			rows.Close()
			return st, err
		}
		st.Confirmed[asset], _ = new(big.Int).SetString(amt, 10)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT status, coalesce(reject_reason, ''), count(*) FROM withdrawal_requests WHERE status IN ('failed', 'rejected') GROUP BY 1, 2`)
	if err != nil {
		return st, fmt.Errorf("failure reasons: %w", err)
	}
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.Status, &f.Reason, &f.Count); err != nil {
			rows.Close()
			return st, err
		}
		st.Failures = append(st.Failures, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	if err := tx.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(extract(epoch FROM broadcast_at - created_at)), 0), coalesce(sum(extract(epoch FROM confirmed_at - broadcast_at)), 0)
		FROM withdrawal_requests WHERE status = 'confirmed' AND broadcast_at IS NOT NULL AND confirmed_at IS NOT NULL`).
		Scan(&st.TimedCount, &st.ProcessingSecs, &st.ChainWaitSecs); err != nil {
		return st, fmt.Errorf("timings: %w", err)
	}

	for status, age := range map[string]time.Duration{StatusRequested: th.Request, StatusValidated: th.Reservation, StatusReserved: th.Reservation, StatusSigned: th.Reservation} {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE status = $1 AND updated_at < now() - make_interval(secs => $2)`, status, age.Seconds()).Scan(&n); err != nil {
			return st, fmt.Errorf("stuck %s: %w", status, err)
		}
		st.Stuck[status] = n
	}

	for _, status := range nonTerminal {
		st.OldestAge[status] = 0
	}
	rows, err = tx.Query(ctx, `SELECT status, extract(epoch FROM now() - min(updated_at)) FROM withdrawal_requests
		WHERE status IN ('requested', 'validated', 'reserved', 'signed', 'broadcast', 'disputed') GROUP BY status`)
	if err != nil {
		return st, fmt.Errorf("oldest ages: %w", err)
	}
	for rows.Next() {
		var status string
		var age float64
		if err := rows.Scan(&status, &age); err != nil {
			rows.Close()
			return st, err
		}
		st.OldestAge[status] = age
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE reported_at IS NULL) FROM nonce_gaps`).Scan(&st.NonceGaps, &st.NonceGapsUnreported); err != nil {
		return st, fmt.Errorf("nonce gaps: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM whitelist_entries WHERE blocked_at IS NOT NULL`).Scan(&st.WhitelistBlocked); err != nil {
		return st, fmt.Errorf("blocked whitelist entries: %w", err)
	}
	var oldestOutbox *float64
	if err := tx.QueryRow(ctx, `SELECT count(*), extract(epoch FROM now() - min(created_at)) FROM withdrawal_outbox WHERE published_at IS NULL`).Scan(&st.OutboxUnpublished, &oldestOutbox); err != nil {
		return st, fmt.Errorf("outbox: %w", err)
	}
	if oldestOutbox != nil && *oldestOutbox > 0 {
		st.OutboxOldestAge = *oldestOutbox
	}
	return st, nil
}

// StuckBroadcast counts broadcast requests the Reaper first saw at least `blocks` blocks ago: the chain head the
// Reaper reads, minus broadcast_head_seen.
func (s *Store) StuckBroadcast(ctx context.Context, head, blocks int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE status = 'broadcast' AND broadcast_head_seen IS NOT NULL AND $1 - broadcast_head_seen >= $2`, head, blocks).Scan(&n)
	return n, err
}
