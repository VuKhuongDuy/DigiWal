package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestPoolCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	st := PoolStat{Max: 10, Idle: 3, Acquired: 7, AcquireCount: 100, EmptyAcquireCount: 4, AcquireDuration: 1500 * time.Millisecond}
	reg.MustRegister(PoolCollector("ledger-service", func() PoolStat { return st }))
	out := exposition(reg)
	for _, want := range []string{
		`digiwal_db_pool_max_connections{service="ledger-service"} 10`,
		`digiwal_db_pool_connections{service="ledger-service",state="acquired"} 7`, `digiwal_db_pool_connections{service="ledger-service",state="idle"} 3`,
		"# TYPE digiwal_db_pool_acquires_total counter", `digiwal_db_pool_acquires_total{service="ledger-service"} 100`,
		`digiwal_db_pool_empty_acquires_total{service="ledger-service"} 4`, `digiwal_db_pool_acquire_seconds_total{service="ledger-service"} 1.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// read at scrape time: a changed pool shows on the next scrape
	st.Acquired = 9
	if out := exposition(reg); !strings.Contains(out, `state="acquired"} 9`) {
		t.Errorf("not live:\n%s", out)
	}
}

type fakePgx struct{}

func (fakePgx) MaxConns() int32                { return 4 }
func (fakePgx) IdleConns() int32               { return 1 }
func (fakePgx) AcquiredConns() int32           { return 3 }
func (fakePgx) AcquireCount() int64            { return 50 }
func (fakePgx) EmptyAcquireCount() int64       { return 2 }
func (fakePgx) AcquireDuration() time.Duration { return time.Second }

func TestPoolFromPgx(t *testing.T) {
	s := PoolFromPgx(fakePgx{})
	if s.Max != 4 || s.Idle != 1 || s.Acquired != 3 || s.Total != 4 || s.AcquireCount != 50 || s.EmptyAcquireCount != 2 || s.AcquireDuration != time.Second {
		t.Fatalf("%+v", s)
	}
}
