// Package metrics is the shared Prometheus plumbing for every DigiWal service
// (docs/design/monitoring-dashboard.md, task 1): a registry with the Go and process collectors, a
// listener that serves /metrics and nothing else, and a scrape-time collector helper.
//
// Rules the helpers enforce for every service (the design's invariants 3 and 4):
//   - a failed read yields no sample, never a zero or a stale value, and drops digiwal_collector_up;
//   - a metric is read-only: collectors only query, they never write;
//   - labels are the caller's responsibility, but the allow-list below is checked in tests via CheckLabels.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultPort is the internal metrics port of every service (METRICS_PORT). It is never published.
const DefaultPort = "9100"

// AllowedLabels are the only label names a DigiWal metric may carry. Never an identifier (user id, address,
// tx hash, withdrawal id): cardinality, and it would leak identifiers into a store with no access control.
var AllowedLabels = map[string]bool{
	"asset": true, "wallet": true, "status": true, "kind": true, "rule": true, "day": true, "chain_id": true,
	"service": true, "job": true, "result": true, "flow": true, "severity": true, "component": true,
	"collector": true, "reason": true, "state": true, "caller": true, "purpose": true, "route": true, "code": true, "stage": true,
}

// CheckLabels returns an error if any name is not on the allow-list.
func CheckLabels(names ...string) error {
	for _, n := range names {
		if !AllowedLabels[n] {
			return fmt.Errorf("label %q is not allowed on a DigiWal metric", n)
		}
	}
	return nil
}

// NewRegistry is a registry with the Go runtime and process collectors already registered.
func NewRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

// Handler serves the registry in the Prometheus text format.
func Handler(r *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(r, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Serve listens on addr (":9100") and serves GET /metrics, and nothing else, until ctx is done. No business
// endpoint is ever added here (design invariant 6).
func Serve(ctx context.Context, addr string, r *prometheus.Registry) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler(r))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return err
		}
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// Gauge, Counter and Untyped build a sample for a Collect func. The values are read from a store at scrape
// time, so a "counter" here is a monotonic figure derived from an append-only table, not an in-process tally.
func Gauge(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func Counter(d *prometheus.Desc, v float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
}

// NewDesc is prometheus.NewDesc with the label allow-list enforced; it panics on a forbidden label, which is a
// programming error caught the first time the service starts (and by every service's tests).
func NewDesc(name, help string, labels ...string) *prometheus.Desc {
	if err := CheckLabels(labels...); err != nil {
		panic(fmt.Sprintf("metric %s: %v", name, err))
	}
	return prometheus.NewDesc(name, help, labels, nil)
}

// UpDesc is digiwal_collector_up{collector}: 1 if the collector's last read succeeded, else 0.
var UpDesc = NewDesc("digiwal_collector_up", "1 if the scrape-time collector's last read succeeded, 0 if it failed (its series are then omitted).", "collector")

// PartialError is what a Read returns, together with the samples it did manage to read, when only some of its reads
// failed (one balance among many). The collector serves those samples, omits the rest, and reports
// digiwal_collector_up 0; it does not cache the result, so the failed reads are retried on the next scrape.
type PartialError struct{ Err error }

func (p *PartialError) Error() string { return "partial read: " + p.Err.Error() }
func (p *PartialError) Unwrap() error { return p.Err }

// Partial wraps err as a PartialError.
func Partial(err error) error { return &PartialError{Err: err} }

// Read is what a scrape-time collector does: query its own store (or the RPC) and return the samples. It must
// respect ctx: the collector gives it a deadline.
type Read func(ctx context.Context) ([]prometheus.Metric, error)

// Cached is a prometheus.Collector that runs a Read at scrape time, at most once per TTL, with a deadline.
//
// A successful read is served from cache until the TTL expires. A failed read (error or timeout) is NOT cached
// and NOT papered over with the previous value: its series are omitted and collector_up is 0, so a dashboard
// shows a gap instead of a number that is quietly wrong (design invariant 4).
type Cached struct {
	name    string
	ttl     time.Duration
	timeout time.Duration
	read    Read
	now     func() time.Time

	mu      chan struct{} // 1-slot semaphore: one read at a time, scrapers wait for it
	samples []prometheus.Metric
	partial []prometheus.Metric // what a partially failed read did return
	at      time.Time
	ok      bool
}

// NewCached builds the collector. ttl is how long a good read is reused (10s for on-chain figures, 60s for
// by-day SQL); timeout bounds one read (5s in the design).
func NewCached(name string, ttl, timeout time.Duration, read Read) *Cached {
	c := &Cached{name: name, ttl: ttl, timeout: timeout, read: read, now: time.Now, mu: make(chan struct{}, 1)}
	return c
}

// Describe sends nothing: the collector is unchecked, since which series exist depends on the data.
func (c *Cached) Describe(chan<- *prometheus.Desc) {}

func (c *Cached) Collect(out chan<- prometheus.Metric) {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()

	if !c.ok || c.now().Sub(c.at) >= c.ttl {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		samples, err := c.read(ctx)
		cancel()
		var partial *PartialError
		switch {
		case err == nil:
			c.samples, c.ok, c.at, c.partial = samples, true, c.now(), nil
		case errors.As(err, &partial):
			// serve what was read, but report the failure and read again next time
			c.samples, c.ok, c.partial = samples, false, samples
		default:
			c.samples, c.ok, c.partial = nil, false, nil
		}
	}
	up := 0.0
	if c.ok {
		up = 1
		for _, m := range c.samples {
			out <- m
		}
	} else {
		for _, m := range c.partial {
			out <- m
		}
	}
	out <- Gauge(UpDesc, up, c.name)
}
