package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// Cache counts what a read cache did, per service and cache (docs/design/read-cache.md): requests by result (hit, miss, error) and
// invalidations by result (ok, error). The cache name goes in the "component" label.
type Cache struct {
	service, component              string
	hit, miss, errs, invOK, invFail atomic.Uint64
}

// NewCache returns the counters of one cache; register them with CacheCollector.
func NewCache(service, component string) *Cache {
	return &Cache{service: service, component: component}
}

func (c *Cache) Hit()   { c.hit.Add(1) }
func (c *Cache) Miss()  { c.miss.Add(1) }
func (c *Cache) Error() { c.errs.Add(1) }

// Invalidated counts one delete after a write.
func (c *Cache) Invalidated(ok bool) {
	if ok {
		c.invOK.Add(1)
	} else {
		c.invFail.Add(1)
	}
}

var (
	cacheRequests      = NewDesc("digiwal_cache_requests_total", "Read-cache lookups by result: hit, miss, error (error = Redis failed, the database answered).", "service", "component", "result")
	cacheInvalidations = NewDesc("digiwal_cache_invalidations_total", "Cache deletes after a write by result: ok, error (an error leaves the entry until its TTL).", "service", "component", "result")
)

type cacheCollector struct{ caches []*Cache }

// CacheCollector exposes the counters at scrape time.
func CacheCollector(caches ...*Cache) prometheus.Collector { return &cacheCollector{caches: caches} }

func (cc *cacheCollector) Describe(chan<- *prometheus.Desc) {}

func (cc *cacheCollector) Collect(out chan<- prometheus.Metric) {
	for _, c := range cc.caches {
		out <- Counter(cacheRequests, float64(c.hit.Load()), c.service, c.component, "hit")
		out <- Counter(cacheRequests, float64(c.miss.Load()), c.service, c.component, "miss")
		out <- Counter(cacheRequests, float64(c.errs.Load()), c.service, c.component, "error")
		out <- Counter(cacheInvalidations, float64(c.invOK.Load()), c.service, c.component, "ok")
		out <- Counter(cacheInvalidations, float64(c.invFail.Load()), c.service, c.component, "error")
	}
}
