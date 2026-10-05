package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	testDesc    = NewDesc("digiwal_test_things", "test", "asset")
	testCounter = NewDesc("digiwal_test_events_total", "test", "asset")
)

func scrape(t *testing.T, r *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestCachedServesSamplesAndUp(t *testing.T) {
	r := prometheus.NewRegistry()
	r.MustRegister(NewCached("things", time.Minute, time.Second, func(context.Context) ([]prometheus.Metric, error) {
		return []prometheus.Metric{Gauge(testDesc, 7, "ETH"), Counter(testCounter, 3, "TKN")}, nil
	}))
	out := scrape(t, r)
	for _, want := range []string{`digiwal_test_things{asset="ETH"} 7`, `digiwal_test_events_total{asset="TKN"} 3`, `# TYPE digiwal_test_events_total counter`, `digiwal_collector_up{collector="things"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Design invariant 4: a failed read is a gap, not a zero and not the previous value.
func TestFailedReadOmitsTheSeriesAndDropsUp(t *testing.T) {
	r := prometheus.NewRegistry()
	fail := false
	c := NewCached("things", 0, time.Second, func(context.Context) ([]prometheus.Metric, error) { // ttl 0: read every scrape
		if fail {
			return nil, errors.New("db down")
		}
		return []prometheus.Metric{Gauge(testDesc, 7, "ETH")}, nil
	})
	r.MustRegister(c)
	if !strings.Contains(scrape(t, r), `digiwal_test_things{asset="ETH"} 7`) {
		t.Fatal("first scrape should have the series")
	}
	fail = true
	out := scrape(t, r)
	if strings.Contains(out, "digiwal_test_things") {
		t.Errorf("a failed read must not serve the old value:\n%s", out)
	}
	if !strings.Contains(out, `digiwal_collector_up{collector="things"} 0`) {
		t.Errorf("collector_up should be 0:\n%s", out)
	}
	fail = false
	if out := scrape(t, r); !strings.Contains(out, `digiwal_test_things{asset="ETH"} 7`) || !strings.Contains(out, `up{collector="things"} 1`) {
		t.Errorf("it should recover:\n%s", out)
	}
}

func TestCachingAndTTL(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := NewCached("x", 10*time.Second, time.Second, func(context.Context) ([]prometheus.Metric, error) {
		calls++
		return []prometheus.Metric{Gauge(testDesc, float64(calls), "ETH")}, nil
	})
	c.now = func() time.Time { return now }
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	scrape(t, r)
	now = now.Add(9 * time.Second)
	scrape(t, r)
	if calls != 1 {
		t.Fatalf("within the TTL the read is reused, calls = %d", calls)
	}
	now = now.Add(2 * time.Second)
	scrape(t, r)
	if calls != 2 {
		t.Fatalf("after the TTL it reads again, calls = %d", calls)
	}
}

// A failed read is retried on the next scrape, it is not cached for the TTL.
func TestFailureIsNotCached(t *testing.T) {
	calls := 0
	c := NewCached("x", time.Hour, time.Second, func(context.Context) ([]prometheus.Metric, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return []prometheus.Metric{Gauge(testDesc, 1, "ETH")}, nil
	})
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	scrape(t, r)
	if out := scrape(t, r); !strings.Contains(out, `digiwal_test_things{asset="ETH"} 1`) || calls != 2 {
		t.Fatalf("calls %d:\n%s", calls, out)
	}
}

func TestReadTimeout(t *testing.T) {
	c := NewCached("slow", 0, 30*time.Millisecond, func(ctx context.Context) ([]prometheus.Metric, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return []prometheus.Metric{Gauge(testDesc, 1, "ETH")}, nil
		}
	})
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	start := time.Now()
	out := scrape(t, r)
	if time.Since(start) > time.Second || !strings.Contains(out, `up{collector="slow"} 0`) || strings.Contains(out, "digiwal_test_things") {
		t.Fatalf("a slow read must time out into a gap (%s):\n%s", time.Since(start), out)
	}
}

func TestLabelAllowList(t *testing.T) {
	if err := CheckLabels("asset", "status", "day"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"user_id", "address", "tx_hash", "withdrawal_id", "idempotency_key"} {
		if CheckLabels(bad) == nil {
			t.Errorf("%s is an identifier and must be refused", bad)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewDesc must panic on label %s", bad)
				}
			}()
			NewDesc("digiwal_x", "x", bad)
		}()
	}
}

func TestRegistryHasRuntimeMetrics(t *testing.T) {
	out := scrape(t, NewRegistry())
	if !strings.Contains(out, "go_goroutines") || !strings.Contains(out, "process_start_time_seconds") {
		t.Errorf("runtime and process collectors missing")
	}
}

// Serve serves /metrics and nothing else, and stops when its context is cancelled.
func TestServeOnlyMetrics(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, NewRegistry()) }()

	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = http.Get("http://" + addr + "/metrics"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "go_goroutines") {
		t.Fatalf("/metrics: %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/healthz", "/v1/anything", "/debug/pprof/"} {
		r, err := http.Get("http://" + addr + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Errorf("%s answered %d, the metrics listener serves only /metrics", p, r.StatusCode)
		}
	}
	if r, _ := http.Post("http://"+addr+"/metrics", "text/plain", nil); r != nil {
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Error("POST /metrics must not be served")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
