package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCacheCollectorCountsByResult(t *testing.T) {
	c := NewCache("svc", "balances")
	c.Hit()
	c.Hit()
	c.Miss()
	c.Error()
	c.Invalidated(true)
	c.Invalidated(false)
	reg := prometheus.NewRegistry()
	reg.MustRegister(CacheCollector(c))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["service"] != "svc" || labels["component"] != "balances" {
				t.Errorf("unexpected labels %v", labels)
			}
			got[mf.GetName()+"/"+labels["result"]] = m.GetCounter().GetValue()
		}
	}
	want := map[string]float64{
		"digiwal_cache_requests_total/hit": 2, "digiwal_cache_requests_total/miss": 1, "digiwal_cache_requests_total/error": 1,
		"digiwal_cache_invalidations_total/ok": 1, "digiwal_cache_invalidations_total/error": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
