package hsm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// handle is a PKCS#11 session handle. It is only ever visible inside Lease.Do's callback.
type handle uint

// ErrLeaseReleased is returned when a Lease is used after Release.
var ErrLeaseReleased = errors.New("hsm: session lease already released")

// Pool owns the PKCS#11 sessions. M0 spike (b): one session used by two goroutines at once makes
// libsofthsm2 SIGSEGV, so the pool makes that structurally impossible: a session handle lives in
// exactly one place, either in the free channel or in exactly one Lease, never both, and the only
// way to touch it is Lease.Do. Nothing else in the process stores a session handle.
type Pool struct {
	free  chan handle
	open  func() (handle, error)
	close func(handle)

	mu     sync.Mutex
	closed bool
}

// newPool opens size sessions through open.
func newPool(size int, open func() (handle, error), closeFn func(handle)) (*Pool, error) {
	if size < 1 {
		return nil, errors.New("hsm: pool size must be >= 1")
	}
	p := &Pool{free: make(chan handle, size), open: open, close: closeFn}
	for i := 0; i < size; i++ {
		h, err := open()
		if err != nil {
			p.Close()
			return nil, err
		}
		p.free <- h
	}
	return p, nil
}

// Acquire blocks until a session is free or ctx ends.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	select {
	case h, ok := <-p.free:
		if !ok {
			return nil, errors.New("hsm: pool closed")
		}
		return &Lease{p: p, h: h}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Quiesce takes every session of the pool, so nothing else uses the token until release is called. Token objects (KEK generate, retire,
// destroy) are created and removed with the pool idle: on SoftHSM a token object created while other sessions run operations can fail with
// CKR_GENERAL_ERROR (found by the KEK-rotation-under-load test). The pause is the length of one key generation. On timeout nothing stays held.
func (p *Pool) Quiesce(ctx context.Context) (release func(), err error) {
	var held []*Lease
	releaseAll := func() {
		for _, l := range held {
			l.Release()
		}
	}
	for i := 0; i < cap(p.free); i++ {
		l, err := p.Acquire(ctx)
		if err != nil {
			releaseAll()
			return nil, err
		}
		held = append(held, l)
	}
	return releaseAll, nil
}

// Close closes every free session. Leased sessions are closed when released.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	for {
		select {
		case h := <-p.free:
			p.close(h)
		default:
			return
		}
	}
}

// Lease is exclusive use of one session until Release.
type Lease struct {
	p    *Pool
	h    handle
	done atomic.Bool
	busy atomic.Bool
}

// Do runs fn with the leased session handle. Calls on one Lease cannot overlap or follow Release.
func (l *Lease) Do(fn func(h handle) error) error {
	if l.done.Load() {
		return ErrLeaseReleased
	}
	if !l.busy.CompareAndSwap(false, true) {
		return errors.New("hsm: concurrent use of one session lease")
	}
	defer l.busy.Store(false)
	return fn(l.h)
}

// Reopen replaces a dead session handle with a fresh one (login state is token-wide, so no
// C_Login is needed). On failure the lease is dropped from the pool and the pool shrinks by one.
func (l *Lease) Reopen() error {
	if l.done.Load() {
		return ErrLeaseReleased
	}
	l.p.close(l.h)
	h, err := l.p.open()
	if err != nil {
		l.Drop()
		return err
	}
	l.h = h
	return nil
}

// Release returns the session to the pool (idempotent).
func (l *Lease) Release() {
	if !l.done.CompareAndSwap(false, true) {
		return
	}
	l.p.mu.Lock()
	closed := l.p.closed
	l.p.mu.Unlock()
	if closed {
		l.p.close(l.h)
		return
	}
	l.p.free <- l.h
}

// Drop discards the session without returning it (the pool shrinks).
func (l *Lease) Drop() {
	if l.done.CompareAndSwap(false, true) {
		l.p.close(l.h)
	}
}
