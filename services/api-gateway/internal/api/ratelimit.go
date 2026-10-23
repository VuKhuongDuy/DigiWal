package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter is an approximate sliding-window rate limiter: per key it keeps two counters (the
// current and previous fixed window) and weights the previous one by how much of it still
// overlaps the sliding window. That is ~32 bytes of state per active key, with an error well
// under the limit itself — the cheap end of the accuracy/memory trade-off, on purpose.
//
// State is in-process, so with N gateway instances the effective limit is up to N× the
// configured one.
type Limiter struct {
	max    float64
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]*counter
	lastSweep time.Time
}

type counter struct {
	start     time.Time // start of the current fixed window
	cur, prev float64
}

// NewLimiter allows about limit requests per key per window.
func NewLimiter(limit int, per time.Duration) *Limiter {
	return &Limiter{max: float64(limit), window: per, now: time.Now, keys: map[string]*counter{}}
}

// Allow records a request for key and reports whether it is within the limit. When it is not,
// retryAfter is how long until the key is likely to be allowed again.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	w := l.keys[key]
	if w == nil {
		w = &counter{start: now}
		l.keys[key] = w
	}
	switch elapsed := now.Sub(w.start); {
	case elapsed >= 2*l.window: // idle for a whole window: nothing carries over
		w.start, w.prev, w.cur = now, 0, 0
	case elapsed >= l.window:
		w.start, w.prev, w.cur = w.start.Add(l.window), w.cur, 0
	}
	frac := float64(now.Sub(w.start)) / float64(l.window)
	if w.prev*(1-frac)+w.cur+1 > l.max {
		return false, l.window - now.Sub(w.start)
	}
	w.cur++
	return true, 0
}

// sweep drops keys idle for two windows, at most once per window, so memory follows the set of
// recently active keys rather than every key ever seen. Caller holds l.mu.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, w := range l.keys {
		if now.Sub(w.start) >= 2*l.window {
			delete(l.keys, k)
		}
	}
}

// limit rejects with 429 once key(r) is over the limiter's budget. A nil limiter lets
// everything through.
func (l *Limiter) limit(next http.Handler, key func(*http.Request) string) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := l.Allow(key(r)); !ok {
			secs := int((retry + time.Second - 1) / time.Second)
			w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
