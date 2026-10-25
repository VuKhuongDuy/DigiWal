// Package obs is sweep-signer's metrics: refusals per rule, the Address Audit's state and the number of Seed unseals.
// Nothing key-related, no address, no user id (monitoring-dashboard.md invariant 3): only counts and flags.
package obs

import (
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"digiwal/pkg/metrics"
)

// Audit is what the collector reads from the Address Audit.
type Audit interface {
	Frozen() string
	Mismatches() int
	Checked() uint64
	LastOK() time.Time
	Ready() error
}

// Seeds is what the collector reads from the Seed provider.
type Seeds interface {
	Unseals() uint64
	Unlocked() uint64
}

var (
	refusals   = metrics.NewDesc("sweep_signer_refusals_total", "Sweeps refused, by rule (key-custody-hsm.md invariant 15). Every refusal also logs an ALERT.", "rule")
	frozenDesc = metrics.NewDesc("sweep_signer_signing_frozen", "1 while the Address Audit has frozen all signing (a mismatch), else 0.")
	readyDesc  = metrics.NewDesc("sweep_signer_ready", "1 once the first Address Audit pass is clean and no mismatch has been seen, else 0.")
	mismatches = metrics.NewDesc("sweep_signer_audit_mismatches_total", "Address Audit mismatches seen since start.")
	checked    = metrics.NewDesc("sweep_signer_audit_rows_checked_total", "wallet_addresses rows re-derived from the Seed by the Address Audit since start.")
	lastOK     = metrics.NewDesc("sweep_signer_audit_last_ok_timestamp_seconds", "Unix time of the last clean Address Audit pass (absent before the first).")
	unseals    = metrics.NewDesc("sweep_signer_seed_unseals_total", "Times the Seed was opened (Sweeps plus audit passes).")
	unlocked   = metrics.NewDesc("sweep_signer_seed_pages_unlocked_total", "Seed pages that mlock could not lock (swap protection missing).")
)

// Observer counts refusals and is the Prometheus collector.
type Observer struct {
	audit Audit
	seeds Seeds

	mu  sync.Mutex
	ref map[string]uint64
}

// New builds the collector.
func New(a Audit, s Seeds) *Observer { return &Observer{audit: a, seeds: s, ref: map[string]uint64{}} }

// Refused implements sweep.Observer.
func (o *Observer) Refused(rule string) {
	o.mu.Lock()
	o.ref[rule]++
	o.mu.Unlock()
}

// Count is the refusal count of a rule (tests).
func (o *Observer) Count(rule string) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ref[rule]
}

func (o *Observer) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{refusals, frozenDesc, readyDesc, mismatches, checked, lastOK, unseals, unlocked} {
		ch <- d
	}
}

func (o *Observer) Collect(ch chan<- prometheus.Metric) {
	o.mu.Lock()
	rules := make([]string, 0, len(o.ref))
	for r := range o.ref {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	for _, r := range rules {
		ch <- metrics.Counter(refusals, float64(o.ref[r]), r)
	}
	o.mu.Unlock()
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	ch <- metrics.Gauge(frozenDesc, b(o.audit.Frozen() != ""))
	ch <- metrics.Gauge(readyDesc, b(o.audit.Ready() == nil))
	ch <- metrics.Counter(mismatches, float64(o.audit.Mismatches()))
	ch <- metrics.Counter(checked, float64(o.audit.Checked()))
	if t := o.audit.LastOK(); !t.IsZero() {
		ch <- metrics.Gauge(lastOK, float64(t.Unix()))
	}
	ch <- metrics.Counter(unseals, float64(o.seeds.Unseals()))
	ch <- metrics.Counter(unlocked, float64(o.seeds.Unlocked()))
}
