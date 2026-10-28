package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) (addr string, hits *atomic.Int64) {
	t.Helper()
	hits = new(atomic.Int64)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); h(w, r) }))
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://"), hits
}

func client(b *Balancer) *http.Client { return &http.Client{Transport: b, Timeout: 5 * time.Second} }

func get(t *testing.T, c *http.Client) int {
	t.Helper()
	resp, err := c.Get("http://ledger-service/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func newRT() http.RoundTripper { return http.DefaultTransport.(*http.Transport).Clone() }

func TestBalancerRewritesHostAndSpreadsRequests(t *testing.T) {
	a, ha := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	b2, hb := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	c := client(NewBalancer([]string{a, b2}, newRT, time.Second))
	for i := 0; i < 200; i++ {
		get(t, c)
	}
	if ha.Load()+hb.Load() != 200 || ha.Load() < 40 || hb.Load() < 40 {
		t.Fatalf("expected a roughly even split of 200, got %d and %d", ha.Load(), hb.Load())
	}
}

func TestBalancerPrefersTheLessBusyInstance(t *testing.T) {
	release := make(chan struct{})
	slow, hs := serve(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	fast, hf := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	c := client(NewBalancer([]string{slow, fast}, newRT, time.Second))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // park requests on the slow one, then see where new ones go
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := c.Get("http://ledger-service/slow"); err == nil {
				resp.Body.Close()
			}
		}()
	}
	for hs.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	before := hs.Load()
	for i := 0; i < 50; i++ {
		get(t, c)
	}
	close(release)
	wg.Wait()
	if hf.Load() < 40 || hs.Load()-before > 10 {
		t.Fatalf("busy instance should get few new requests: slow +%d, fast %d", hs.Load()-before, hf.Load())
	}
}

func TestBalancerLeavesOutADeadInstanceAndRetriesGet(t *testing.T) {
	live, hl := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close() // nothing listens there now
	c := client(NewBalancer([]string{deadAddr, live}, newRT, time.Minute))
	for i := 0; i < 30; i++ {
		if code := get(t, c); code != 200 { // a GET that lands on the dead one is retried on the live one
			t.Fatalf("status %d", code)
		}
	}
	if hl.Load() != 30 {
		t.Fatalf("live instance should have answered all 30, got %d", hl.Load())
	}
}

func TestBalancerDoesNotRetryAPost(t *testing.T) {
	live, hl := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	b := NewBalancer([]string{deadAddr}, newRT, time.Minute)
	b.eps = append(b.eps, &endpoint{addr: live, rt: newRT()})
	b.eps[1].inflight.Store(1000) // make the dead one the first choice
	c := client(b)
	resp, err := c.Post("http://ledger-service/x", "text/plain", strings.NewReader("x"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("a POST must not be retried on another instance")
	}
	if hl.Load() != 0 {
		t.Fatalf("live instance saw %d requests", hl.Load())
	}
}

type failing struct{ err error }

func (f failing) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestBalancerRecoversAfterTheCooldown(t *testing.T) {
	live, hl := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	b := NewBalancer([]string{live}, newRT, 50*time.Millisecond)
	b.eps[0].rt = &flaky{next: newRT(), failures: 1}
	c := client(b)
	if _, err := c.Get("http://ledger-service/x"); err == nil {
		t.Fatal("the first call should fail")
	}
	if b.up(b.eps[0], time.Now().UnixNano()) {
		t.Fatal("the instance should be marked down")
	}
	time.Sleep(80 * time.Millisecond)
	if get(t, c) != 200 || hl.Load() != 1 {
		t.Fatal("the instance should be used again after the cooldown")
	}
}

type flaky struct {
	next     http.RoundTripper
	failures int
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("connection refused")
	}
	return f.next.RoundTrip(r)
}
