// Package obs is the auditor's metrics (docs/design/reconciliation-audit.md, "New metrics"): the history of its own
// jobs, the suspicious transactions it holds, and the reconciliation figures. All read from the auditor's own
// database at scrape time, so "last run" survives a restart (invariant 7) and a failed read is a gap, not a zero.
// Counts only: no user id, address, tx hash or reference ever becomes a label (invariant 6).
package obs

import (
	"context"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/auditor/internal/store"

	"github.com/prometheus/client_golang/prometheus"
)

// Source is the auditor's store.
type Source interface {
	Metrics(ctx context.Context) (store.Metrics, error)
}

var (
	lastRun      = metrics.NewDesc("digiwal_job_last_run_timestamp_seconds", "When the job last finished a run (Unix seconds), whatever its result.", "job")
	lastSuccess  = metrics.NewDesc("digiwal_job_last_success_timestamp_seconds", "When the job last finished ok or warn. Absent if it never did; an error is never a success.", "job")
	lastDuration = metrics.NewDesc("digiwal_job_last_duration_seconds", "How long its last run took.", "job")
	interval     = metrics.NewDesc("digiwal_job_interval_seconds", "How often the job is meant to run, for 'stale' alerts (no run for 3 intervals).", "job")
	lastResult   = metrics.NewDesc("digiwal_job_last_result", "1 for the result of the job's last run (ok, warn, fail or error).", "job", "result")
	open         = metrics.NewDesc("digiwal_suspicious_open", "Open suspicious transactions.", "flow", "rule", "severity")
	oldestOpen   = metrics.NewDesc("digiwal_suspicious_oldest_open_age_seconds", "Age of the oldest open suspicious transaction of a severity; 0 when none.", "severity")
	detected     = metrics.NewDesc("digiwal_suspicious_detected_total", "Suspicious transactions ever recorded (rows are never deleted).", "flow", "rule", "severity")
	reconCustody = metrics.NewDesc("digiwal_recon_custody", "Latest reconciliation figures per asset, whole units: on_chain, ledger, explained, residual.", "asset", "component")
)

var results = []string{"ok", "warn", "fail", "error"}

// Collector exports the auditor's own state. intervals is each registered job's period, from the scheduler.
func Collector(s Source, intervals map[string]time.Duration) prometheus.Collector {
	return metrics.NewCached("auditor", 15*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		m, err := s.Metrics(ctx)
		if err != nil {
			return nil, err
		}
		var out []prometheus.Metric
		for job, d := range intervals {
			out = append(out, metrics.Gauge(interval, d.Seconds(), job))
		}
		for _, j := range m.Jobs {
			out = append(out,
				metrics.Gauge(lastRun, float64(j.LastRunAt.Unix()), j.Job),
				metrics.Gauge(lastDuration, j.LastDuration.Seconds(), j.Job))
			if j.LastSuccess != nil {
				out = append(out, metrics.Gauge(lastSuccess, float64(j.LastSuccess.Unix()), j.Job))
			}
			for _, r := range results { // one series per result, 1 for the current one
				v := 0.0
				if r == j.LastResult {
					v = 1
				}
				out = append(out, metrics.Gauge(lastResult, v, j.Job, r))
			}
		}
		for _, c := range m.Open {
			out = append(out, metrics.Gauge(open, float64(c.Count), c.Flow, c.Rule, c.Severity))
		}
		for sev, age := range m.OldestOpenAge {
			out = append(out, metrics.Gauge(oldestOpen, age, sev))
		}
		for _, c := range m.Detected {
			out = append(out, metrics.Counter(detected, float64(c.Count), c.Flow, c.Rule, c.Severity))
		}
		for _, f := range m.Recon {
			out = append(out,
				metrics.Gauge(reconCustody, f.OnChain, f.Asset, "on_chain"), metrics.Gauge(reconCustody, f.LedgerCustody, f.Asset, "ledger"),
				metrics.Gauge(reconCustody, f.Explained, f.Asset, "explained"), metrics.Gauge(reconCustody, f.Residual, f.Asset, "residual"))
		}
		return out, nil
	})
}
