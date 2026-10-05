package obs

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

func scrape(reg *prometheus.Registry) string {
	rec := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestEverySeriesExistsFromTheStart(t *testing.T) {
	reg := prometheus.NewRegistry()
	New(reg)
	out := scrape(reg)
	for _, want := range []string{
		`digiwal_sweeper_inflight{kind="gas_topup"} 0`, `digiwal_sweeper_inflight{kind="sweep"} 0`,
		`digiwal_sweeper_inflight_oldest_age_seconds{kind="sweep"} 0`, `digiwal_sweeper_fee_bumps_total{kind="gas_topup"} 0`,
		`digiwal_sweeper_alerts_total{kind="bump_exhausted"} 0`, `digiwal_sweeper_alerts_total{kind="gas_fee_report_failed"} 0`, "digiwal_sweeper_events_abandoned_total 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestInflightAgesAreReadAtScrapeTime(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New(reg)
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }

	doneA := s.Start("sweep")
	clock = clock.Add(30 * time.Second)
	doneB := s.Start("sweep")
	doneT := s.Start("gas_topup")
	clock = clock.Add(10 * time.Second)

	out := scrape(reg)
	for _, want := range []string{`digiwal_sweeper_inflight{kind="sweep"} 2`, `digiwal_sweeper_inflight_oldest_age_seconds{kind="sweep"} 40`,
		`digiwal_sweeper_inflight{kind="gas_topup"} 1`, `digiwal_sweeper_inflight_oldest_age_seconds{kind="gas_topup"} 10`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	doneA(true) // the oldest one ends: the oldest age drops to the next one's
	out = scrape(reg)
	if !strings.Contains(out, `digiwal_sweeper_inflight{kind="sweep"} 1`) || !strings.Contains(out, `digiwal_sweeper_inflight_oldest_age_seconds{kind="sweep"} 10`) {
		t.Errorf("\n%s", out)
	}
	doneB(false) // failed: leaves the in-flight set, but is not a completion time
	doneT(true)
	out = scrape(reg)
	if !strings.Contains(out, `digiwal_sweeper_inflight{kind="sweep"} 0`) || !strings.Contains(out, `digiwal_sweeper_inflight_oldest_age_seconds{kind="sweep"} 0`) {
		t.Errorf("\n%s", out)
	}
	if !strings.Contains(out, `digiwal_sweeper_completion_seconds_count{kind="sweep"} 1`) || !strings.Contains(out, `digiwal_sweeper_completion_seconds_sum{kind="sweep"} 40`) ||
		!strings.Contains(out, `digiwal_sweeper_completion_seconds_count{kind="gas_topup"} 1`) {
		t.Errorf("only successful completions are timed:\n%s", out)
	}
}

func TestCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := New(reg)
	s.FeeBump("gas_topup")
	s.FeeBump("gas_topup")
	s.Alerted("bump_refused")
	s.Abandoned()
	out := scrape(reg)
	for _, want := range []string{`digiwal_sweeper_fee_bumps_total{kind="gas_topup"} 2`, `digiwal_sweeper_alerts_total{kind="bump_refused"} 1`, "digiwal_sweeper_events_abandoned_total 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
