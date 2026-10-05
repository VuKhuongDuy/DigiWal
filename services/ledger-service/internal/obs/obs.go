// Package obs is ledger-service's metrics (docs/design/monitoring-dashboard.md section 5; reconciliation-audit.md
// decision (c)). It reads only ledger-service's own database, at scrape time.
package obs

import (
	"context"
	"math/big"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/ledger-service/internal/ledger"

	"github.com/prometheus/client_golang/prometheus"
)

// Source is the ledger store.
type Source interface {
	Stats(ctx context.Context) (ledger.Stats, error)
}

var (
	depositsCredited = metrics.NewDesc("digiwal_deposits_credited_total", "Deposits credited to users, ever (monotonic).", "asset")
	depositAmount    = metrics.NewDesc("digiwal_deposit_amount_total", "Amount credited by deposits, ever, in whole asset units (monotonic). Display only.", "asset")
	activeUserDays   = metrics.NewDesc("digiwal_ledger_active_user_days_total", "Pairs of (UTC day, user) with ledger activity, ever (monotonic); its increase over a day is that day's active users.")
	liabilities      = metrics.NewDesc("digiwal_ledger_user_liabilities", "Sum of user balances (what the ledger owes users), whole asset units. Ledger figure.", "asset")
	failedEvents     = metrics.NewDesc("digiwal_ledger_failed_events", "Unprocessable events parked in failed_events.")
	flagged          = metrics.NewDesc("digiwal_ledger_flagged_deposits", "Deposits from Screening List senders, by review status.", "status")
	activeLocks      = metrics.NewDesc("digiwal_ledger_active_locks", "Active withdrawal Locks.")
	oldestLockAge    = metrics.NewDesc("digiwal_ledger_active_locks_oldest_age_seconds", "Age of the oldest active Lock; 0 when none.")
)

// Whole turns minor units into whole asset units for display (design invariant 5: never feeds logic).
func Whole(minor *big.Int, decimals int) float64 {
	if minor == nil {
		return 0
	}
	f := new(big.Float).SetInt(minor)
	f.Quo(f, new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	v, _ := f.Float64()
	return v
}

// Collector is the scrape-time collector for ledger-service.
func Collector(s Source) prometheus.Collector {
	return metrics.NewCached("ledger-service", 30*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		st, err := s.Stats(ctx)
		if err != nil {
			return nil, err
		}
		var out []prometheus.Metric
		for _, a := range st.Assets {
			out = append(out,
				metrics.Counter(depositsCredited, float64(a.DepositsCredited), a.Symbol),
				metrics.Counter(depositAmount, Whole(a.DepositAmount, a.Decimals), a.Symbol),
				metrics.Gauge(liabilities, Whole(a.UserLiabilities, a.Decimals), a.Symbol))
		}
		out = append(out,
			metrics.Counter(activeUserDays, float64(st.ActiveUserDays)),
			metrics.Gauge(failedEvents, float64(st.FailedEvents)),
			metrics.Gauge(activeLocks, float64(st.ActiveLocks)),
			metrics.Gauge(oldestLockAge, st.OldestLockAge.Seconds()))
		for status, n := range st.Flagged {
			out = append(out, metrics.Gauge(flagged, float64(n), status))
		}
		return out, nil
	})
}
