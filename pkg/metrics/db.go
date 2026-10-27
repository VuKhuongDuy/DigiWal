package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DB holds the histograms a service fills from its database driver's hooks (for pgx: services/*/internal/*/trace.go), so that pkg/metrics stays
// free of the driver. They show what the service itself sees, as opposed to what the server reports in pg_stat_statements:
//
//	digiwal_db_pool_acquire_wait_seconds  time to get a connection from the pool (includes dialling a new one)
//	digiwal_db_pool_hold_seconds          time a connection stays checked out, from acquire to release
//	digiwal_db_query_duration_seconds     one statement, from sending it to the last row read
type DB struct{ wait, hold, query prometheus.Histogram }

var dbBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// NewDB registers the histograms on reg.
func NewDB(reg prometheus.Registerer, service string) *DB {
	if err := CheckLabels("service"); err != nil {
		panic(err)
	}
	cl := prometheus.Labels{"service": service}
	h := func(name, help string) prometheus.Histogram {
		return prometheus.NewHistogram(prometheus.HistogramOpts{Name: name, Help: help, ConstLabels: cl, Buckets: dbBuckets})
	}
	d := &DB{
		wait:  h("digiwal_db_pool_acquire_wait_seconds", "Time to get a connection from the pool, including dialling a new one."),
		hold:  h("digiwal_db_pool_hold_seconds", "Time a connection stays checked out of the pool, from acquire to release."),
		query: h("digiwal_db_query_duration_seconds", "One statement as the service sees it: from sending it to the last row read."),
	}
	reg.MustRegister(d.wait, d.hold, d.query)
	return d
}

func (d *DB) ObserveAcquireWait(t time.Duration) { d.wait.Observe(t.Seconds()) }
func (d *DB) ObserveHold(t time.Duration)        { d.hold.Observe(t.Seconds()) }
func (d *DB) ObserveQuery(t time.Duration)       { d.query.Observe(t.Seconds()) }
