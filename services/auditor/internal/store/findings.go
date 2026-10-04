package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"digiwal/services/auditor/internal/finding"

	"github.com/jackc/pgx/v5"
)

// Reconcile records the outcome of evaluating `rules` for one reference (docs/design/reconciliation-audit.md
// invariants 4 and 5), in one transaction:
//
//   - violated, not seen before        -> remembered in pending_findings (two-pass), unless Immediate;
//   - violated, pending or Immediate   -> upserted into suspicious_transactions (open);
//   - violated, already recorded       -> detail refreshed, a resolved row reopens, a warn that has been
//     open longer than escalateAfter becomes its EscalateTo severity;
//   - not violated                     -> its pending entry is dropped, an open row becomes resolved.
//
// An acknowledged row stays acknowledged while the violation persists. Rows are never deleted.
func (s *Store) Reconcile(ctx context.Context, runID int64, flow, ref string, rules []string, vs []finding.Violation, now time.Time, escalateAfter time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, rule := range rules {
		var v *finding.Violation
		for i := range vs {
			if vs[i].Rule == rule {
				v = &vs[i]
			}
		}
		if err := reconcileRule(ctx, tx, runID, flow, ref, rule, v, now, escalateAfter); err != nil {
			return fmt.Errorf("%s %s %s: %w", flow, ref, rule, err)
		}
	}
	return tx.Commit(ctx)
}

func reconcileRule(ctx context.Context, tx pgx.Tx, runID int64, flow, ref, rule string, v *finding.Violation, now time.Time, escalateAfter time.Duration) error {
	var (
		rowID       int64
		status, sev string
		detectedAt  time.Time
		haveRow     bool
	)
	err := tx.QueryRow(ctx, `SELECT id, status, severity, detected_at FROM suspicious_transactions
		WHERE flow = $1 AND reference = $2 AND rule = $3 FOR UPDATE`, flow, ref, rule).Scan(&rowID, &status, &sev, &detectedAt)
	switch {
	case err == nil:
		haveRow = true
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	var pendingRun int64
	err = tx.QueryRow(ctx, `SELECT first_run_id FROM pending_findings WHERE flow = $1 AND reference = $2 AND rule = $3`, flow, ref, rule).Scan(&pendingRun)
	havePending := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if v == nil { // not violated now
		if havePending {
			if _, err := tx.Exec(ctx, `DELETE FROM pending_findings WHERE flow = $1 AND reference = $2 AND rule = $3`, flow, ref, rule); err != nil {
				return err
			}
		}
		if haveRow && status == "open" {
			_, err := tx.Exec(ctx, `UPDATE suspicious_transactions SET status = 'resolved', resolved_at = $2, last_run_id = $3 WHERE id = $1`, rowID, now, runID)
			return err
		}
		return nil
	}

	detail, err := json.Marshal(v.Detail)
	if err != nil {
		return err
	}
	severity := v.Severity

	if haveRow {
		if v.EscalateTo != "" && now.Sub(detectedAt) >= escalateAfter {
			severity = v.EscalateTo
		}
		if sev == finding.Critical { // never downgrade
			severity = finding.Critical
		}
		newStatus, resolvedAt, detected := status, any(nil), detectedAt
		if status == "resolved" { // it came back
			newStatus, detected = "open", now
		}
		if status == "acknowledged" {
			resolvedAt = nil
		}
		_, err = tx.Exec(ctx, `UPDATE suspicious_transactions SET status = $2, severity = $3, detail = $4, last_run_id = $5,
			detected_at = $6, resolved_at = $7 WHERE id = $1`, rowID, newStatus, severity, detail, runID, detected, resolvedAt)
		return err
	}

	if v.Immediate || havePending {
		first := runID
		if havePending {
			first = pendingRun
		}
		if _, err := tx.Exec(ctx, `INSERT INTO suspicious_transactions (flow, reference, rule, severity, detail, first_run_id, last_run_id, detected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, flow, ref, rule, severity, detail, first, runID, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM pending_findings WHERE flow = $1 AND reference = $2 AND rule = $3`, flow, ref, rule)
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO pending_findings (flow, reference, rule, severity, detail, first_run_id, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, flow, ref, rule, severity, detail, runID, now)
	return err
}

// OpenRefs lists the references of a flow that need re-evaluating on every run: those with a pending
// entry, an open row, or a warn/acknowledged row that may still change.
func (s *Store) OpenRefs(ctx context.Context, flow string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT reference FROM pending_findings WHERE flow = $1
		UNION SELECT reference FROM suspicious_transactions WHERE flow = $1 AND status IN ('open','acknowledged')
		ORDER BY 1`, flow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountOpen is the number of open suspicious transactions of a flow by severity (what a run reports).
func (s *Store) CountOpen(ctx context.Context, flow string) (warn, critical int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE severity = 'warn'), count(*) FILTER (WHERE severity = 'critical')
		FROM suspicious_transactions WHERE flow = $1 AND status = 'open'`, flow).Scan(&warn, &critical)
	return
}

// Cursor returns where a job stopped (last_id), or "" if it never ran.
func (s *Store) Cursor(ctx context.Context, name string) (string, error) {
	var id *string
	err := s.pool.QueryRow(ctx, `SELECT last_id FROM audit_cursor WHERE job = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return "", nil
	}
	return *id, err
}

func (s *Store) SetCursor(ctx context.Context, name, lastID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_cursor (job, last_id, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (job) DO UPDATE SET last_id = EXCLUDED.last_id, updated_at = EXCLUDED.updated_at`, name, lastID, at)
	return err
}

// Finding is one row of suspicious_transactions, as read back.
type Finding struct {
	Flow, Reference, Rule, Severity, Status string
}

// Findings lists a flow's suspicious transactions, oldest first.
func (s *Store) Findings(ctx context.Context, flow string) ([]Finding, error) {
	rows, err := s.pool.Query(ctx, `SELECT flow, reference, rule, severity, status FROM suspicious_transactions WHERE flow = $1 ORDER BY id`, flow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.Flow, &f.Reference, &f.Rule, &f.Severity, &f.Status); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Movement is a row of treasury_movements.
type Movement struct {
	ChainID                int64
	TxHash                 string
	LogIndex               int64
	Asset                  string
	Amount                 string
	From, To               string
	FromTier, ToTier, Kind string
	Block                  int64
	Time                   time.Time
}

// AddMovement records a Treasury Movement; the same transfer scanned again is left as it is.
func (s *Store) AddMovement(ctx context.Context, m Movement) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO treasury_movements
		(chain_id, tx_hash, log_index, asset, amount, from_address, to_address, from_tier, to_tier, kind, block_number, block_time)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
		m.ChainID, m.TxHash, m.LogIndex, m.Asset, m.Amount, m.From, m.To, m.FromTier, m.ToTier, m.Kind, m.Block, m.Time)
	return err
}

// Movements lists the recorded Treasury Movements, oldest first.
func (s *Store) Movements(ctx context.Context) ([]Movement, error) {
	rows, err := s.pool.Query(ctx, `SELECT chain_id, tx_hash, log_index, asset, amount::text, from_address, to_address,
		from_tier, to_tier, kind, block_number, block_time FROM treasury_movements ORDER BY block_number, tx_hash, log_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Movement
	for rows.Next() {
		var m Movement
		if err := rows.Scan(&m.ChainID, &m.TxHash, &m.LogIndex, &m.Asset, &m.Amount, &m.From, &m.To, &m.FromTier, &m.ToTier, &m.Kind, &m.Block, &m.Time); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
