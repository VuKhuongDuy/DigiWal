package api

import (
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Balancer is an http.RoundTripper that spreads requests over several instances of one service, deciding per REQUEST (client-side load balancing).
//
// Why not a layer-4 balancer in front: the calls go over HTTP/2, which carries a great many requests on a few long-lived connections, and a TCP
// balancer can only spread connections. Measured 2026-10-27 with HAProxy in mode tcp: 6,332 and 6,482 connections, but 21,441 and 197,788 requests
// on the two ledger-services. A layer-7 balancer would have to terminate TLS, and ledger-service authorises callers by the CN of the client
// certificate, so the gateway talks to the instances itself, each over its own HTTP/2 transport (own connections, own mTLS handshake).
//
// Choice: power of two choices on requests in flight (take two instances at random, send to the one with fewer). An instance that fails to
// answer at all (connection refused, reset, timeout) is left out for cooldown; idempotent requests are retried once on another instance.
type Balancer struct {
	eps      []*endpoint
	cooldown time.Duration
	mu       sync.Mutex // guards rnd
	rnd      *rand.Rand
}

type endpoint struct {
	addr      string // host:port the request is sent to
	rt        http.RoundTripper
	inflight  atomic.Int64
	downUntil atomic.Int64 // unix nanoseconds
}

// NewBalancer makes one transport per address with newRT. cooldown is how long a failing instance is left out.
func NewBalancer(addrs []string, newRT func() http.RoundTripper, cooldown time.Duration) *Balancer {
	b := &Balancer{cooldown: cooldown, rnd: rand.New(rand.NewSource(time.Now().UnixNano()))}
	for _, a := range addrs {
		b.eps = append(b.eps, &endpoint{addr: a, rt: newRT()})
	}
	return b
}

func (b *Balancer) up(e *endpoint, now int64) bool { return e.downUntil.Load() <= now }

// pick returns an instance that is up (any, if all are down), never one of skip.
func (b *Balancer) pick(skip *endpoint) *endpoint {
	now := time.Now().UnixNano()
	var c []*endpoint
	for _, e := range b.eps {
		if e != skip && b.up(e, now) {
			c = append(c, e)
		}
	}
	if len(c) == 0 {
		for _, e := range b.eps {
			if e != skip {
				c = append(c, e)
			}
		}
	}
	switch len(c) {
	case 0:
		return nil
	case 1:
		return c[0]
	}
	b.mu.Lock()
	i := b.rnd.Intn(len(c))
	j := b.rnd.Intn(len(c) - 1)
	b.mu.Unlock()
	if j >= i {
		j++
	}
	if c[j].inflight.Load() < c[i].inflight.Load() {
		return c[j]
	}
	return c[i]
}

func (b *Balancer) RoundTrip(req *http.Request) (*http.Response, error) {
	e := b.pick(nil)
	resp, err := b.send(e, req)
	if err != nil && idempotent(req) && len(b.eps) > 1 && req.Context().Err() == nil {
		if o := b.pick(e); o != nil {
			resp, err = b.send(o, req)
		}
	}
	return resp, err
}

func (b *Balancer) send(e *endpoint, req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context()) // a RoundTripper must not modify the request it was given
	r.URL.Host = e.addr
	e.inflight.Add(1)
	resp, err := e.rt.RoundTrip(r)
	if err != nil {
		e.inflight.Add(-1)
		if req.Context().Err() == nil { // the caller giving up says nothing about the instance
			e.downUntil.Store(time.Now().Add(b.cooldown).UnixNano())
		}
		return nil, err
	}
	resp.Body = &countedBody{ReadCloser: resp.Body, done: func() { e.inflight.Add(-1) }} // in flight until the body is read
	return resp, nil
}

func idempotent(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}

type countedBody struct {
	io.ReadCloser
	done func()
	once sync.Once
}

func (c *countedBody) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.done)
	return err
}
