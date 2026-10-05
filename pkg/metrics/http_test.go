package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func serve(t *testing.T) (*http.ServeMux, http.Handler, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /items", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) { time.Sleep(60 * time.Millisecond) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) { panic("handler bug") })
	mux.HandleFunc("GET /twice", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.WriteHeader(http.StatusTeapot) // a second WriteHeader is ignored by net/http; so must it be here
	})
	return mux, NewHTTP(reg, "demo").Wrap(mux), reg
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func exposition(reg *prometheus.Registry) string {
	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestRouteIsThePatternNotThePath(t *testing.T) {
	_, h, reg := serve(t)
	for _, id := range []string{"1", "2", "3", "a-very-long-id-0123456789"} {
		do(h, "GET", "/items/"+id)
	}
	do(h, "POST", "/items")
	out := exposition(reg)
	if !strings.Contains(out, `digiwal_http_request_duration_seconds_count{code="200",route="GET /items/{id}",service="demo"} 4`) {
		t.Errorf("four ids, one series:\n%s", out)
	}
	if strings.Contains(out, "a-very-long-id") || strings.Contains(out, `route="GET /items/1"`) {
		t.Errorf("a path leaked into a label:\n%s", out)
	}
	if !strings.Contains(out, `code="201",route="POST /items"`) {
		t.Errorf("status code from WriteHeader:\n%s", out)
	}
}

func TestUnmatchedRequestsShareOneRoute(t *testing.T) {
	_, h, reg := serve(t)
	for _, p := range []string{"/nope", "/nope/1", "/wp-admin", "/x/y/z"} {
		do(h, "GET", p)
	}
	out := exposition(reg)
	if !strings.Contains(out, `code="404",route="unmatched",service="demo"} 4`) || strings.Contains(out, "wp-admin") {
		t.Errorf("probing must not create series:\n%s", out)
	}
}

func TestStatusDefaultsAndFirstWriteHeaderWins(t *testing.T) {
	_, h, reg := serve(t)
	do(h, "GET", "/items/9") // handler only writes a body: 200
	do(h, "GET", "/twice")
	out := exposition(reg)
	if !strings.Contains(out, `code="200",route="GET /items/{id}"`) || !strings.Contains(out, `code="202",route="GET /twice"`) || strings.Contains(out, `code="418"`) {
		t.Errorf("\n%s", out)
	}
}

func TestDurationIsObserved(t *testing.T) {
	_, h, reg := serve(t)
	do(h, "GET", "/slow")
	out := exposition(reg)
	if !strings.Contains(out, `route="GET /slow",service="demo",le="0.05"} 0`) || !strings.Contains(out, `route="GET /slow",service="demo",le="0.1"} 1`) {
		t.Errorf("a 60ms request belongs in the 100ms bucket, not the 50ms one:\n%s", out)
	}
}

// A panic is a 500 for the client; it must be counted as one and still propagate to the server's own recovery.
func TestPanicIsCountedAsAServerError(t *testing.T) {
	_, h, reg := serve(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must propagate")
			}
		}()
		do(h, "GET", "/boom")
	}()
	if out := exposition(reg); !strings.Contains(out, `code="500",route="GET /boom"`) {
		t.Errorf("\n%s", out)
	}
}

func TestWrapperStaysTransparent(t *testing.T) {
	flushed := false
	reg := prometheus.NewRegistry()
	h := NewHTTP(reg, "demo").Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController must reach the real writer: %v", err)
		}
	}))
	do(h, "GET", "/")
	if !flushed {
		t.Error("Flusher must be passed through")
	}
}

func TestRouteAndCodeAreAllowedLabels(t *testing.T) {
	if err := CheckLabels("route", "code", "stage"); err != nil {
		t.Fatal(err)
	}
}
