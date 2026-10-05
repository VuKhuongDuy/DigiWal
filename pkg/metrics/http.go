package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTP measures every request a service answers: how long it took and how it ended, per route
// (docs/design/reconciliation-audit.md, "Performance"). The route is the ServeMux pattern ("POST /v1/withdrawals"),
// never the raw path, so ids in a URL cannot multiply series; a request no pattern matched is "unmatched".
//
// It measures server-side time, from the first byte read to the handler returning. Health and metrics routes are
// in the data like any other: the dashboards exclude them when they check the latency targets.
type HTTP struct{ hist *prometheus.HistogramVec }

// Buckets cover the targets (p50 50ms, p95 200ms, p99 2s) with room on both sides.
var httpBuckets = []float64{.005, .01, .025, .05, .1, .2, .5, 1, 2, 5, 10, 30}

// NewHTTP registers digiwal_http_request_duration_seconds{service,route,code} on reg.
func NewHTTP(reg prometheus.Registerer, service string) *HTTP {
	if err := CheckLabels("route", "code", "service"); err != nil {
		panic(err)
	}
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "digiwal_http_request_duration_seconds", Help: "Server-side time to answer a request, by route and status code. Its _count is the request count.",
		ConstLabels: prometheus.Labels{"service": service}, Buckets: httpBuckets,
	}, []string{"route", "code"})
	reg.MustRegister(h)
	return &HTTP{hist: h}
}

// Wrap measures next.
func (h *HTTP) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			code := rec.code
			if p := recover(); p != nil { // the server answers 500 for a panic; count it as one, then let it propagate
				code = http.StatusInternalServerError
				h.observe(r, code, start)
				panic(p)
			}
			h.observe(r, code, start)
		}()
		next.ServeHTTP(rec, r)
	})
}

func (h *HTTP) observe(r *http.Request, code int, start time.Time) {
	route := r.Pattern // set by the ServeMux that routed the request
	if route == "" {
		route = "unmatched"
	}
	h.hist.WithLabelValues(route, strconv.Itoa(code)).Observe(time.Since(start).Seconds())
}

// statusWriter records the status code and stays transparent for Flush, Hijack and ResponseController.
type statusWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.code, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("the underlying ResponseWriter does not support hijacking")
}
