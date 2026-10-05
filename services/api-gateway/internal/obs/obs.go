// Package obs is api-gateway's metrics (docs/design/monitoring-dashboard.md, section 2; reconciliation-audit.md
// decision (c)). It reads only api-gateway's own database.
package obs

import (
	"context"

	"digiwal/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
	"time"
)

// Users counts users.
type Users interface {
	CountUsers(ctx context.Context) (int64, error)
}

var usersTotal = metrics.NewDesc("digiwal_users_total", "Users ever created. Monotonic; new users per day is increase() over a day.")

// Collector is the scrape-time collector for api-gateway.
func Collector(u Users) prometheus.Collector {
	return metrics.NewCached("api-gateway", 30*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		n, err := u.CountUsers(ctx)
		if err != nil {
			return nil, err
		}
		return []prometheus.Metric{metrics.Counter(usersTotal, float64(n))}, nil
	})
}
