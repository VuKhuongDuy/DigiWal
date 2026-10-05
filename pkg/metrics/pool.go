package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PoolStat is the part of a connection pool's state that matters for saturation. A service fills it from its pool
// (for pgx: pool.Stat()); keeping it a plain struct keeps pkg/metrics free of a database driver.
type PoolStat struct {
	Max, Total, Idle, Acquired int32
	AcquireCount               int64         // acquires, ever
	EmptyAcquireCount          int64         // acquires that had to wait for a connection, ever
	AcquireDuration            time.Duration // total time spent acquiring, ever
}

var (
	poolMax      = NewDesc("digiwal_db_pool_max_connections", "Configured maximum connections of the database pool.", "service")
	poolTotal    = NewDesc("digiwal_db_pool_connections", "Connections the pool holds, by state: idle, acquired.", "service", "state")
	poolAcquired = NewDesc("digiwal_db_pool_acquires_total", "Connection acquires, ever.", "service")
	poolWaited   = NewDesc("digiwal_db_pool_empty_acquires_total", "Acquires that had to wait because no connection was free, ever. Rises when the pool is saturated.", "service")
	poolWait     = NewDesc("digiwal_db_pool_acquire_seconds_total", "Total time spent acquiring connections, ever.", "service")
)

type poolCollector struct {
	service string
	stat    func() PoolStat
}

// PoolCollector reads the pool at scrape time (it is in-process, so no cache or deadline is needed). Saturation is
// acquired / max.
func PoolCollector(service string, stat func() PoolStat) prometheus.Collector {
	return &poolCollector{service: service, stat: stat}
}

func (c *poolCollector) Describe(chan<- *prometheus.Desc) {}

func (c *poolCollector) Collect(out chan<- prometheus.Metric) {
	s := c.stat()
	out <- Gauge(poolMax, float64(s.Max), c.service)
	out <- Gauge(poolTotal, float64(s.Idle), c.service, "idle")
	out <- Gauge(poolTotal, float64(s.Acquired), c.service, "acquired")
	out <- Counter(poolAcquired, float64(s.AcquireCount), c.service)
	out <- Counter(poolWaited, float64(s.EmptyAcquireCount), c.service)
	out <- Counter(poolWait, s.AcquireDuration.Seconds(), c.service)
}
