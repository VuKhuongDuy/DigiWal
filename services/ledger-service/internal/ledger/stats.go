package ledger

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// AssetStats are one asset's figures for the dashboards.
type AssetStats struct {
	ID, Symbol       string
	Decimals         int
	DepositsCredited int64    // deposit Ledger Transactions ever posted (monotonic)
	DepositAmount    *big.Int // sum of what they credited, minor units (monotonic)
	UserLiabilities  *big.Int // sum of user Accounts' total_balance now
}

// Stats is what ledger-service exports as metrics (docs/design/monitoring-dashboard.md section 5,
// reconciliation-audit.md decision (c)). Read-only, in one snapshot so the figures agree with each other.
type Stats struct {
	Assets         []AssetStats
	ActiveUserDays int64            // (UTC day, user) pairs with an Entry or Lock ever: monotonic; its increase over a day is that day's active users
	FailedEvents   int64            // parked unprocessable events
	Flagged        map[string]int64 // Flagged Deposits by status (pending, approved, rejected)
	ActiveLocks    int64
	OldestLockAge  time.Duration // age of the oldest active Lock, 0 if none
}

// Stats reads the figures. A Flagged Deposit status with no rows is reported as 0, so its series does not vanish.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Flagged: map[string]int64{"pending": 0, "approved": 0, "rejected": 0}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	rows, err := tx.Query(ctx, `
		SELECT a.id::text, a.symbol, a.decimals,
		       (SELECT count(DISTINCT e.ledger_transaction_id) FROM entries e
		          JOIN accounts ac ON ac.id = e.account_id AND ac.kind = 'user' AND ac.asset_id = a.id
		          JOIN ledger_transactions t ON t.id = e.ledger_transaction_id AND t.source_type = 'deposit'),
		       (SELECT coalesce(sum(e.amount), 0)::text FROM entries e
		          JOIN accounts ac ON ac.id = e.account_id AND ac.kind = 'user' AND ac.asset_id = a.id
		          JOIN ledger_transactions t ON t.id = e.ledger_transaction_id AND t.source_type = 'deposit'),
		       (SELECT coalesce(sum(ac.total_balance), 0)::text FROM accounts ac WHERE ac.asset_id = a.id AND ac.kind = 'user')
		FROM assets a ORDER BY a.symbol, a.id`)
	if err != nil {
		return st, fmt.Errorf("asset stats: %w", err)
	}
	for rows.Next() {
		var a AssetStats
		var dep, liab string
		if err := rows.Scan(&a.ID, &a.Symbol, &a.Decimals, &a.DepositsCredited, &dep, &liab); err != nil {
			rows.Close()
			return st, err
		}
		a.DepositAmount, _ = new(big.Int).SetString(dep, 10)
		a.UserLiabilities, _ = new(big.Int).SetString(liab, 10)
		st.Assets = append(st.Assets, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT date_trunc('day', e.created_at AT TIME ZONE 'UTC') AS d, ac.user_id
		    FROM entries e JOIN accounts ac ON ac.id = e.account_id AND ac.kind = 'user'
		  UNION
		  SELECT date_trunc('day', l.created_at AT TIME ZONE 'UTC'), ac.user_id
		    FROM locks l JOIN accounts ac ON ac.id = l.account_id) t`).Scan(&st.ActiveUserDays); err != nil {
		return st, fmt.Errorf("active user days: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM failed_events`).Scan(&st.FailedEvents); err != nil {
		return st, fmt.Errorf("failed events: %w", err)
	}
	fr, err := tx.Query(ctx, `SELECT status, count(*) FROM flagged_deposits GROUP BY status`)
	if err != nil {
		return st, fmt.Errorf("flagged deposits: %w", err)
	}
	for fr.Next() {
		var status string
		var n int64
		if err := fr.Scan(&status, &n); err != nil {
			fr.Close()
			return st, err
		}
		st.Flagged[status] = n
	}
	fr.Close()
	if err := fr.Err(); err != nil {
		return st, err
	}
	var age *float64
	if err := tx.QueryRow(ctx, `SELECT count(*), extract(epoch FROM now() - min(created_at)) FROM locks WHERE status = 'active'`).Scan(&st.ActiveLocks, &age); err != nil {
		return st, fmt.Errorf("active locks: %w", err)
	}
	if age != nil && *age > 0 {
		st.OldestLockAge = time.Duration(*age * float64(time.Second))
	}
	return st, nil
}
