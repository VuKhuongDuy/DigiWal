package metrics

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Upstream measures the calls this service makes to other services, from the client side: what the caller waits, which the callee's own
// digiwal_http_request_duration_seconds cannot show (it leaves out the balancer, the network, TLS and the callee's accept queue).
//
//	digiwal_upstream_request_duration_seconds{kind,code}  time from sending the request to the response headers (code "error" if no response)
//	digiwal_upstream_connections_total{kind,result}       connections used per request: result "new" (dialled for this request) or "reused"
//	digiwal_upstream_setup_seconds{kind,stage}            what a new connection costs: stage "connect" (TCP) and "tls" (handshake)
//
// kind names the callee ("ledger", "wallet", "withdrawal").
type Upstream struct {
	dur   *prometheus.HistogramVec
	conns *prometheus.CounterVec
	setup *prometheus.HistogramVec
}

var upstreamBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// NewUpstream registers the three metrics on reg.
func NewUpstream(reg prometheus.Registerer, service string) *Upstream {
	if err := CheckLabels("kind", "code", "result", "stage", "service"); err != nil {
		panic(err)
	}
	cl := prometheus.Labels{"service": service}
	u := &Upstream{
		dur: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "digiwal_upstream_request_duration_seconds",
			Help: "Client-side time of a call to another service, until the response headers arrived.", ConstLabels: cl, Buckets: upstreamBuckets}, []string{"kind", "code"}),
		conns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "digiwal_upstream_connections_total",
			Help: "Calls to another service by whether they used a new or a reused connection.", ConstLabels: cl}, []string{"kind", "result"}),
		setup: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "digiwal_upstream_setup_seconds",
			Help: "Time to set up a new connection to another service: TCP connect and TLS handshake.", ConstLabels: cl, Buckets: upstreamBuckets}, []string{"kind", "stage"}),
	}
	reg.MustRegister(u.dur, u.conns, u.setup)
	return u
}

// Transport wraps next so that every call is measured under kind.
func (u *Upstream) Transport(kind string, next http.RoundTripper) http.RoundTripper {
	return &upstreamTransport{u: u, kind: kind, next: next}
}

type upstreamTransport struct {
	u    *Upstream
	kind string
	next http.RoundTripper
}

func (t *upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var connectAt, tlsAt atomic.Int64 // the callbacks may run on the dialling goroutine
	since := func(a *atomic.Int64) float64 { return time.Duration(time.Now().UnixNano() - a.Load()).Seconds() }
	trace := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			res := "new"
			if i.Reused {
				res = "reused"
			}
			t.u.conns.WithLabelValues(t.kind, res).Inc()
		},
		ConnectStart: func(_, _ string) { connectAt.Store(time.Now().UnixNano()) },
		ConnectDone: func(_, _ string, err error) {
			if err == nil && connectAt.Load() != 0 {
				t.u.setup.WithLabelValues(t.kind, "connect").Observe(since(&connectAt))
			}
		},
		TLSHandshakeStart: func() { tlsAt.Store(time.Now().UnixNano()) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil && tlsAt.Load() != 0 {
				t.u.setup.WithLabelValues(t.kind, "tls").Observe(since(&tlsAt))
			}
		},
	}
	start := time.Now()
	resp, err := t.next.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	code := "error"
	if err == nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	t.u.dur.WithLabelValues(t.kind, code).Observe(time.Since(start).Seconds())
	return resp, err
}
