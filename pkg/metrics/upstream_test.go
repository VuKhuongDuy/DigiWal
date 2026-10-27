package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestUpstreamTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	reg := prometheus.NewRegistry()
	u := NewUpstream(reg, "api-gateway")
	c := &http.Client{Transport: u.Transport("ledger", http.DefaultTransport.(*http.Transport).Clone()), Timeout: 2 * time.Second}
	for _, p := range []string{"/", "/", "/boom"} {
		resp, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	bad := &http.Client{Transport: u.Transport("ledger", http.DefaultTransport), Timeout: time.Second}
	if _, err := bad.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected a connection error")
	}
	out := exposition(reg)
	for _, want := range []string{
		`digiwal_upstream_request_duration_seconds_count{code="200",kind="ledger",service="api-gateway"} 2`,
		`digiwal_upstream_request_duration_seconds_count{code="500",kind="ledger",service="api-gateway"} 1`,
		`digiwal_upstream_request_duration_seconds_count{code="error",kind="ledger",service="api-gateway"} 1`,
		`digiwal_upstream_connections_total{kind="ledger",result="new",service="api-gateway"} 1`,
		`digiwal_upstream_connections_total{kind="ledger",result="reused",service="api-gateway"} 2`,
		`digiwal_upstream_setup_seconds_count{kind="ledger",service="api-gateway",stage="connect"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s\n%s", want, out)
		}
	}
}

func TestDBHistograms(t *testing.T) {
	reg := prometheus.NewRegistry()
	d := NewDB(reg, "ledger-service")
	d.ObserveAcquireWait(3 * time.Millisecond)
	d.ObserveHold(40 * time.Millisecond)
	d.ObserveQuery(time.Millisecond)
	out := exposition(reg)
	for _, want := range []string{
		`digiwal_db_pool_acquire_wait_seconds_count{service="ledger-service"} 1`,
		`digiwal_db_pool_hold_seconds_count{service="ledger-service"} 1`,
		`digiwal_db_query_duration_seconds_count{service="ledger-service"} 1`,
		`digiwal_db_query_duration_seconds_bucket{service="ledger-service",le="0.001"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s\n%s", want, out)
		}
	}
}
