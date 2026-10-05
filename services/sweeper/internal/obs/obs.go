// Package obs is sweeper's metrics (monitoring-dashboard.md section 5). sweeper keeps no database ("keeps no state"),
// so everything here lives in the process and restarts with it, which is acceptable: in-flight work is short-lived
// and a restart re-consumes Kafka. It implements sweep.Observer.
package obs

import (
	"sync"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/sweeper/internal/sweep"

	"github.com/prometheus/client_golang/prometheus"
)

// Kinds of work, and of ALERT (closed sets, also the label values).
var (
	workKinds  = []string{"gas_topup", "sweep"}
	alertKinds = []string{"bump_exhausted", "bump_refused", "cancelled_by_operator", "gas_fee_report_failed"}
)

// completionBuckets run from a block or two to the inclusion timeout and beyond (a stuck top-up is Fee Bumped).
var completionBuckets = []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1200}

type Sweeper struct {
	mu       sync.Mutex
	next     int64
	inflight map[string]map[int64]time.Time // kind -> id -> started
	now      func() time.Time

	bumps     *prometheus.CounterVec
	alerts    *prometheus.CounterVec
	abandoned prometheus.Counter
	took      *prometheus.HistogramVec

	inflightDesc, oldestDesc *prometheus.Desc
}

// New registers sweeper's metrics on reg.
func New(reg prometheus.Registerer) *Sweeper {
	if err := metrics.CheckLabels("kind"); err != nil {
		panic(err)
	}
	s := &Sweeper{
		inflight: map[string]map[int64]time.Time{"gas_topup": {}, "sweep": {}}, now: time.Now,
		bumps:        prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_sweeper_fee_bumps_total", Help: "Fee Bumps of stuck Gas Top-Ups, ever (since the process started)."}, []string{"kind"}),
		alerts:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_sweeper_alerts_total", Help: "ALERTs raised by sweeper, by kind."}, []string{"kind"}),
		abandoned:    prometheus.NewCounter(prometheus.CounterOpts{Name: "digiwal_sweeper_events_abandoned_total", Help: "deposits.confirmed records given up on (never succeed, or MAX_ATTEMPTS failures); funds stay at the user address."}),
		took:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "digiwal_sweeper_completion_seconds", Help: "Time from a Gas Top-Up or Sweep being sent to it being mined, successful ones only.", Buckets: completionBuckets}, []string{"kind"}),
		inflightDesc: metrics.NewDesc("digiwal_sweeper_inflight", "Gas Top-Ups and Sweeps sent and not yet mined.", "kind"),
		oldestDesc:   metrics.NewDesc("digiwal_sweeper_inflight_oldest_age_seconds", "Age of the oldest one in flight; 0 when none.", "kind"),
	}
	s.bumps.WithLabelValues("gas_topup")
	for _, k := range alertKinds { // every series exists from the start, so increase() over a window is right
		s.alerts.WithLabelValues(k)
	}
	reg.MustRegister(s.bumps, s.alerts, s.abandoned, s.took, s)
	return s
}

func (s *Sweeper) Start(kind string) func(ok bool) {
	s.mu.Lock()
	s.next++
	id := s.next
	started := s.now()
	if s.inflight[kind] == nil {
		s.inflight[kind] = map[int64]time.Time{}
	}
	s.inflight[kind][id] = started
	s.mu.Unlock()
	return func(ok bool) {
		s.mu.Lock()
		delete(s.inflight[kind], id)
		s.mu.Unlock()
		if ok {
			s.took.WithLabelValues(kind).Observe(s.now().Sub(started).Seconds())
		}
	}
}

func (s *Sweeper) FeeBump(kind string) { s.bumps.WithLabelValues(kind).Inc() }
func (s *Sweeper) Alerted(kind string) { s.alerts.WithLabelValues(kind).Inc() }
func (s *Sweeper) Abandoned()          { s.abandoned.Inc() }

func (s *Sweeper) Describe(chan<- *prometheus.Desc) {}

// Collect reads the in-flight set at scrape time, so the oldest age is the age now, not when it started.
func (s *Sweeper) Collect(out chan<- prometheus.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, kind := range workKinds {
		var oldest time.Duration
		for _, started := range s.inflight[kind] {
			if age := now.Sub(started); age > oldest {
				oldest = age
			}
		}
		out <- metrics.Gauge(s.inflightDesc, float64(len(s.inflight[kind])), kind)
		out <- metrics.Gauge(s.oldestDesc, oldest.Seconds(), kind)
	}
}

var _ sweep.Observer = (*Sweeper)(nil)
