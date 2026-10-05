// Package obs is withdrawal-service's metrics (monitoring-dashboard.md section 5; reconciliation-audit.md decision
// (c)). It reads only withdrawal-service's own database, plus the chain head for the one figure that needs it.
package obs

import (
	"context"
	"math/big"
	"sync/atomic"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/withdrawal-service/internal/pipeline"
	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/validate"

	"github.com/prometheus/client_golang/prometheus"
)

// Source is the store.
type Source interface {
	Stats(ctx context.Context, th store.StuckThresholds) (store.Stats, error)
	StuckBroadcast(ctx context.Context, head, blocks int64) (int64, error)
}

// Head is the chain head the Reaper reads.
type Head interface {
	Head(ctx context.Context) (int64, error)
}

var (
	withdrawalsTotal = metrics.NewDesc("digiwal_withdrawals_total", "Withdrawal requests that reached a status, ever: requested (all recorded), confirmed, failed, rejected. A confirmed request later refunded moves to failed, the one case a figure can fall.", "status")
	amountTotal      = metrics.NewDesc("digiwal_withdrawal_amount_total", "Amount of confirmed withdrawals, whole asset units. Display only.", "asset")
	failuresTotal    = metrics.NewDesc("digiwal_withdrawal_failures_total", "Failed and rejected requests by reason (a closed set; anything else is 'other').", "status", "reason")
	procSum          = metrics.NewDesc("digiwal_withdrawal_processing_seconds_sum", "Sum, over confirmed withdrawals, of our own time (recorded to broadcast).")
	chainSum         = metrics.NewDesc("digiwal_withdrawal_chain_wait_seconds_sum", "Sum, over confirmed withdrawals, of the time waiting for the blockchain (broadcast to confirmed to depth).")
	timedCount       = metrics.NewDesc("digiwal_withdrawal_timed_total", "Confirmed withdrawals with both timings; divide the two _sum series by this (rate over rate) for averages.")
	requests         = metrics.NewDesc("digiwal_withdrawal_requests", "Requests currently in each status.", "status")
	stuck            = metrics.NewDesc("digiwal_withdrawal_stuck", "Requests past the Reaper's own timeout for their status (broadcast: blocks since first seen).", "status")
	oldest           = metrics.NewDesc("digiwal_withdrawal_oldest_age_seconds", "Age of the oldest request in each non-terminal status; 0 when none.", "status")
	nonceGaps        = metrics.NewDesc("digiwal_nonce_gaps", "Nonce Gaps recorded: state total, or unreported to wallet-service.", "state")
	whitelistBlocked = metrics.NewDesc("digiwal_whitelist_blocked", "Whitelist entries blocked by a screening re-check.")
	outboxBacklog    = metrics.NewDesc("digiwal_outbox_unpublished", "Events written with a state change that Kafka has not acknowledged yet.")
	outboxOldest     = metrics.NewDesc("digiwal_outbox_oldest_unpublished_age_seconds", "Age of the oldest unpublished event; 0 when none. Rising means the publisher or Kafka is behind.")
)

// stageBuckets run from a database round trip to an hour: the in-line stages take milliseconds, the wait for the chain minutes.
var stageBuckets = []float64{.01, .05, .1, .5, 1, 5, 15, 30, 60, 120, 300, 600, 1200, 3600}

// Stages registers digiwal_withdrawal_stage_duration_seconds{stage} on reg and returns the function the store calls
// after each committed status change (Store.ObserveStages). stage is "<from>_to_<to>", a closed set; the value is the
// time spent in <from>. Observed in-process, so it restarts from zero with the service (a counter reset to Prometheus).
func Stages(reg prometheus.Registerer) func(from, to string, took time.Duration) {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "digiwal_withdrawal_stage_duration_seconds", Help: "Time a withdrawal spent in a status before moving to the next one.", Buckets: stageBuckets,
	}, []string{"stage"})
	if err := metrics.CheckLabels("stage"); err != nil {
		panic(err)
	}
	reg.MustRegister(h)
	return func(from, to string, took time.Duration) {
		h.WithLabelValues(from + "_to_" + to).Observe(took.Seconds())
	}
}

// knownReasons is the closed set of reject_reason values; the label is bounded by it (design invariant 3).
var knownReasons = map[string]bool{
	pipeline.ReasonSignRefused: true, pipeline.ReasonTxReverted: true, pipeline.ReasonStaleRequest: true, pipeline.ReasonStaleReservation: true,
	pipeline.ReasonReplacedOrDropped: true, pipeline.ReasonAbandoned: true, pipeline.ReasonAbandonedRefunded: true,
	validate.AssetNotRegistered: true, validate.InvalidToAddress: true, validate.AmountTooLarge: true, validate.BelowMinimum: true,
	validate.NotWhitelisted: true, validate.OnScreeningList: true, validate.DestinationIsDeposit: true, validate.DestinationIsSystem: true,
}

func reasonLabel(r string) string {
	switch {
	case r == "":
		return "none"
	case knownReasons[r]:
		return r
	}
	return "other"
}

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

// Collector exports the database figures. th must be the very values the Reaper is built with.
func Collector(s Source, assets *registry.Registry, th store.StuckThresholds) prometheus.Collector {
	return metrics.NewCached("withdrawal-service", 30*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		st, err := s.Stats(ctx, th)
		if err != nil {
			return nil, err
		}
		var out []prometheus.Metric
		out = append(out,
			metrics.Counter(withdrawalsTotal, float64(st.Created), "requested"),
			metrics.Counter(withdrawalsTotal, float64(st.ByStatus[store.StatusConfirmed]), "confirmed"),
			metrics.Counter(withdrawalsTotal, float64(st.ByStatus[store.StatusFailed]), "failed"),
			metrics.Counter(withdrawalsTotal, float64(st.ByStatus[store.StatusRejected]), "rejected"),
			metrics.Counter(procSum, st.ProcessingSecs), metrics.Counter(chainSum, st.ChainWaitSecs), metrics.Counter(timedCount, float64(st.TimedCount)),
			metrics.Gauge(nonceGaps, float64(st.NonceGaps), "total"), metrics.Gauge(nonceGaps, float64(st.NonceGapsUnreported), "unreported"),
			metrics.Gauge(whitelistBlocked, float64(st.WhitelistBlocked)),
			metrics.Gauge(outboxBacklog, float64(st.OutboxUnpublished)), metrics.Gauge(outboxOldest, st.OutboxOldestAge))

		amounts := map[string]float64{}
		for id, amt := range st.Confirmed {
			label, dec := "unknown", 0
			if a, ok := assets.Get(id); ok {
				label, dec = a.Symbol, a.Decimals
			}
			amounts[label] += Whole(amt, dec)
		}
		for label, v := range amounts {
			out = append(out, metrics.Counter(amountTotal, v, label))
		}

		byReason := map[[2]string]int64{}
		for _, f := range st.Failures {
			byReason[[2]string{f.Status, reasonLabel(f.Reason)}] += f.Count
		}
		for k, n := range byReason {
			out = append(out, metrics.Counter(failuresTotal, float64(n), k[0], k[1]))
		}
		for _, status := range store.AllStatuses {
			out = append(out, metrics.Gauge(requests, float64(st.ByStatus[status]), status))
		}
		for status, n := range st.Stuck {
			out = append(out, metrics.Gauge(stuck, float64(n), status))
		}
		for status, age := range st.OldestAge {
			out = append(out, metrics.Gauge(oldest, age, status))
		}
		return out, nil
	})
}

// BroadcastCollector exports the stuck count for `broadcast`, which needs the chain head. It is separate so a
// failing RPC drops only this series, not the database ones.
func BroadcastCollector(s Source, h Head, blocks int64) prometheus.Collector {
	return metrics.NewCached("withdrawal-service-broadcast", 15*time.Second, 5*time.Second, func(ctx context.Context) ([]prometheus.Metric, error) {
		head, err := h.Head(ctx)
		if err != nil {
			return nil, err
		}
		n, err := s.StuckBroadcast(ctx, head, blocks)
		if err != nil {
			return nil, err
		}
		return []prometheus.Metric{metrics.Gauge(stuck, float64(n), store.StatusBroadcast)}, nil
	})
}

// Reaper counts what the Reaper and the pipeline do, from the hooks in pipeline.Observer. In-process, so the
// counters restart with the process (a counter reset to Prometheus). It implements pipeline.Observer.
type Reaper struct {
	actions *prometheus.CounterVec
	alerts  *prometheus.CounterVec
	lastAt  atomic.Int64 // unix seconds
	desc    *prometheus.Desc
}

// NewReaper registers digiwal_reaper_actions_total{rule}, digiwal_withdrawal_alerts_total{kind} and
// digiwal_reaper_last_pass_timestamp_seconds on reg. The label values come from constants in the pipeline package,
// so the sets are closed.
func NewReaper(reg prometheus.Registerer) *Reaper {
	if err := metrics.CheckLabels("rule", "kind"); err != nil {
		panic(err)
	}
	r := &Reaper{
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_reaper_actions_total", Help: "Stuck requests the Reaper failed, by the reason it recorded."}, []string{"rule"}),
		alerts:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_withdrawal_alerts_total", Help: "ALERTs raised for an operator (after de-duplication), by kind. Reaper and pipeline."}, []string{"kind"}),
		desc:    metrics.NewDesc("digiwal_reaper_last_pass_timestamp_seconds", "When the Reaper last finished a pass (Unix seconds). Absent until its first pass."),
	}
	// every known series exists from the start, so increase() over a window that begins before the first event is right
	for _, rule := range []string{pipeline.ReasonStaleRequest, pipeline.ReasonStaleReservation, pipeline.ReasonReplacedOrDropped} {
		r.actions.WithLabelValues(rule)
	}
	reg.MustRegister(r.actions, r.alerts, r)
	return r
}

func (r *Reaper) Reclaimed(rule string)   { r.actions.WithLabelValues(rule).Inc() }
func (r *Reaper) Alerted(kind string)     { r.alerts.WithLabelValues(kind).Inc() }
func (r *Reaper) ReaperPass(at time.Time) { r.lastAt.Store(at.Unix()) }

func (r *Reaper) Describe(chan<- *prometheus.Desc) {}
func (r *Reaper) Collect(out chan<- prometheus.Metric) {
	if v := r.lastAt.Load(); v != 0 {
		out <- metrics.Gauge(r.desc, float64(v))
	}
}

var _ pipeline.Observer = (*Reaper)(nil)

// Limits exports the Hot Wallet's Floor and Ceiling from the Asset Registry, so a registry edit moves the lines on
// the dashboard (monitoring-dashboard.md invariant 7) and the "below Floor" alert has something to compare to. The
// registry is read once at startup: an edit takes effect on restart.
func Limits(assets *registry.Registry) prometheus.Collector {
	floorDesc := metrics.NewDesc("digiwal_hot_wallet_floor", "Hot Wallet Floor from the Asset Registry, whole asset units.", "asset")
	ceilDesc := metrics.NewDesc("digiwal_hot_wallet_ceiling", "Hot Wallet Ceiling from the Asset Registry, whole asset units.", "asset")
	return &limits{assets: assets, floor: floorDesc, ceil: ceilDesc}
}

type limits struct {
	assets      *registry.Registry
	floor, ceil *prometheus.Desc
}

func (l *limits) Describe(chan<- *prometheus.Desc) {}
func (l *limits) Collect(out chan<- prometheus.Metric) {
	for _, a := range l.assets.All() {
		if a.HotFloor != nil {
			out <- metrics.Gauge(l.floor, Whole(a.HotFloor, a.Decimals), a.Symbol)
		}
		if a.HotCeiling != nil {
			out <- metrics.Gauge(l.ceil, Whole(a.HotCeiling, a.Decimals), a.Symbol)
		}
	}
}
