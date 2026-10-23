package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d rejected within limit", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("4th request: ok=%v retry=%v, want rejected with retry in (0, 1m]", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("a second key must have its own budget")
	}

	// Half a window later the previous window still counts for half: 3*0.5 + 0 + 1 = 2.5 <= 3.
	now = now.Add(90 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("request rejected though the previous window has mostly aged out")
	}
	// Idle for two full windows: a clean slate.
	now = now.Add(3 * time.Minute)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d rejected after idling", i)
		}
	}
}

func TestLimiterSweepsIdleKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(5, time.Minute)
	l.now = func() time.Time { return now }
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	now = now.Add(5 * time.Minute)
	l.Allow("d")
	if len(l.keys) != 1 {
		t.Fatalf("%d keys held, want only the active one", len(l.keys))
	}
}

type noUsers struct{}

func (noUsers) CreateUser(context.Context) (string, string, error) { return "u", "k", nil }

func TestPostUsersIsRateLimitedByIP(t *testing.T) {
	s := &Server{Users: noUsers{}, IPLimit: NewLimiter(2, time.Minute)}
	h := s.Routes()
	do := func(addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/users", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	do("1.2.3.4:1000")
	do("1.2.3.4:2000") // another port, same client
	w := do("1.2.3.4:3000")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d retry-after=%q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	if w := do("5.6.7.8:1000"); w.Code != http.StatusCreated {
		t.Fatalf("other client got %d, want 201", w.Code)
	}

	// Unauthenticated probes are throttled before auth; health checks never are.
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = "1.2.3.4:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("healthz got %d", w.Code)
		}
	}
}
