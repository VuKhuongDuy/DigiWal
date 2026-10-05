package obs_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/auditor/internal/obs"
	"digiwal/services/auditor/internal/store"

	"github.com/prometheus/client_golang/prometheus"
)

type fake struct {
	m   store.Metrics
	err error
}

func (f fake) Metrics(context.Context) (store.Metrics, error) { return f.m, f.err }

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

var ts = time.Unix(1_800_000_000, 0)

func sample() store.Metrics {
	return store.Metrics{
		Jobs: []store.JobStatus{
			{Job: "reconciliation", LastRunAt: ts, LastResult: "error", LastDuration: 2 * time.Second, LastSuccess: &ts},
			{Job: "ledger_integrity", LastRunAt: ts, LastResult: "ok"},
		},
		Open:          []store.OpenCount{{Flow: "withdrawal", Rule: "W1", Severity: "critical", Count: 2}},
		OldestOpenAge: map[string]float64{"critical": 90, "warn": 0, "info": 0},
		Detected:      []store.OpenCount{{Flow: "withdrawal", Rule: "W1", Severity: "critical", Count: 5}},
		Recon:         []store.ReconFigures{{Asset: "ETH", OnChain: 10, LedgerCustody: 9, Explained: 1, Residual: 0}},
	}
}

func TestExports(t *testing.T) {
	out := scrape(obs.Collector(fake{m: sample()}, map[string]time.Duration{"reconciliation": 5 * time.Minute, "never_ran": time.Minute}))
	for _, want := range []string{
		`digiwal_job_last_run_timestamp_seconds{job="reconciliation"} 1.8e+09`,
		`digiwal_job_last_success_timestamp_seconds{job="reconciliation"} 1.8e+09`,
		`digiwal_job_last_duration_seconds{job="reconciliation"} 2`,
		`digiwal_job_interval_seconds{job="reconciliation"} 300`,
		`digiwal_job_interval_seconds{job="never_ran"} 60`, // a registered job that never ran still has an interval: JobStale needs it
		`digiwal_job_last_result{job="reconciliation",result="error"} 1`, `digiwal_job_last_result{job="reconciliation",result="ok"} 0`,
		`digiwal_job_last_result{job="ledger_integrity",result="ok"} 1`,
		`digiwal_suspicious_open{flow="withdrawal",rule="W1",severity="critical"} 2`,
		`digiwal_suspicious_oldest_open_age_seconds{severity="critical"} 90`, `digiwal_suspicious_oldest_open_age_seconds{severity="warn"} 0`,
		"# TYPE digiwal_suspicious_detected_total counter", `digiwal_suspicious_detected_total{flow="withdrawal",rule="W1",severity="critical"} 5`,
		`digiwal_recon_custody{asset="ETH",component="on_chain"} 10`, `digiwal_recon_custody{asset="ETH",component="residual"} 0`,
		`digiwal_collector_up{collector="auditor"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// invariant 2: a job that has only ever errored has no last-success series (not 0, which would read as 1970)
	if strings.Contains(out, `last_success_timestamp_seconds{job="ledger_integrity"}`) {
		t.Errorf("a job with no success must have no last_success series:\n%s", out)
	}
}

func TestUnreadableDatabaseIsAGap(t *testing.T) {
	out := scrape(obs.Collector(fake{err: errors.New("db down")}, map[string]time.Duration{"x": time.Minute}))
	if strings.Contains(out, "digiwal_job_") || !strings.Contains(out, `digiwal_collector_up{collector="auditor"} 0`) {
		t.Fatalf("\n%s", out)
	}
}
