package hsm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/pkcs11"
)

// fakeSessions models libsofthsm2's rule: a session must never be used by two goroutines at once
// (that is a SIGSEGV, not an error). use() fails the test if it ever sees overlap.
type fakeSessions struct {
	t      *testing.T
	mu     sync.Mutex
	next   handle
	live   map[handle]bool
	inUse  map[handle]bool
	opened atomic.Int64
}

func newFake(t *testing.T) *fakeSessions {
	return &fakeSessions{t: t, live: map[handle]bool{}, inUse: map[handle]bool{}}
}
func (f *fakeSessions) open() (handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.live[f.next] = true
	f.opened.Add(1)
	return f.next, nil
}
func (f *fakeSessions) close(h handle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inUse[h] {
		f.t.Errorf("session %d closed while in use", h)
	}
	delete(f.live, h)
}
func (f *fakeSessions) use(h handle) {
	f.mu.Lock()
	if !f.live[h] {
		f.t.Errorf("session %d used but not open", h)
	}
	if f.inUse[h] {
		f.t.Errorf("SESSION %d USED BY TWO GOROUTINES AT ONCE (libsofthsm2 would SIGSEGV)", h)
	}
	f.inUse[h] = true
	f.mu.Unlock()
	time.Sleep(50 * time.Microsecond)
	f.mu.Lock()
	f.inUse[h] = false
	f.mu.Unlock()
}

func TestPool_NeverHandsOneSessionToTwoGoroutines(t *testing.T) {
	f := newFake(t)
	const size = 4
	p, err := newPool(size, f.open, f.close)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var active, peak atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l, err := p.Acquire(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				n := active.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				if err := l.Do(func(h handle) error { f.use(h); return nil }); err != nil {
					t.Error(err)
				}
				active.Add(-1)
				l.Release()
			}
		}()
	}
	wg.Wait()
	if peak.Load() > size {
		t.Fatalf("%d leases at once from a pool of %d", peak.Load(), size)
	}
	t.Logf("64 goroutines x 200 operations over a pool of %d: peak concurrent leases %d, sessions ever opened %d", size, peak.Load(), f.opened.Load())
}

func TestPool_MisuseIsAnErrorNotACrash(t *testing.T) {
	f := newFake(t)
	p, _ := newPool(1, f.open, f.close)
	defer p.Close()
	l, _ := p.Acquire(context.Background())

	// overlapping Do on one lease
	entered, release := make(chan struct{}), make(chan struct{})
	go func() { _ = l.Do(func(handle) error { close(entered); <-release; return nil }) }()
	<-entered
	if err := l.Do(func(handle) error { t.Error("second Do ran"); return nil }); err == nil {
		t.Error("overlapping use of one lease was allowed")
	}
	close(release)
	time.Sleep(10 * time.Millisecond)

	l.Release()
	l.Release() // idempotent: must not put the handle in the pool twice
	if err := l.Do(func(handle) error { return nil }); !errors.Is(err, ErrLeaseReleased) {
		t.Errorf("use after release: %v", err)
	}
	a, _ := p.Acquire(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("a double Release handed the same session out twice")
	}
	a.Release()
}

func TestPool_ReopenReplacesADeadSession(t *testing.T) {
	f := newFake(t)
	p, _ := newPool(1, f.open, f.close)
	defer p.Close()
	l, _ := p.Acquire(context.Background())
	var first handle
	_ = l.Do(func(h handle) error { first = h; return nil })
	if err := l.Reopen(); err != nil {
		t.Fatal(err)
	}
	var second handle
	_ = l.Do(func(h handle) error { second = h; return nil })
	if first == second || f.live[first] {
		t.Fatal("dead session was not replaced and closed")
	}
	l.Release()
}

func TestPool_AcquireHonoursContext(t *testing.T) {
	f := newFake(t)
	p, _ := newPool(1, f.open, f.close)
	defer p.Close()
	l, _ := p.Acquire(context.Background())
	defer l.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestMap_EveryHSMErrorBecomesAnExplicitGatewayError(t *testing.T) {
	for _, c := range []struct {
		ckr  uint
		want string
	}{
		{pkcs11.CKR_DEVICE_ERROR, CodeUnavailable}, {pkcs11.CKR_DEVICE_REMOVED, CodeUnavailable},
		{pkcs11.CKR_TOKEN_NOT_PRESENT, CodeUnavailable}, {pkcs11.CKR_SESSION_CLOSED, CodeUnavailable},
		{pkcs11.CKR_SESSION_HANDLE_INVALID, CodeUnavailable}, {pkcs11.CKR_SESSION_COUNT, CodeUnavailable},
		{pkcs11.CKR_FUNCTION_FAILED, CodeUnavailable},
		{pkcs11.CKR_USER_NOT_LOGGED_IN, CodeNotLoggedIn},
		{pkcs11.CKR_PIN_INCORRECT, CodePIN}, {pkcs11.CKR_PIN_LOCKED, CodePIN},
		{pkcs11.CKR_KEY_HANDLE_INVALID, CodeKeyMissing}, {pkcs11.CKR_OBJECT_HANDLE_INVALID, CodeKeyMissing},
		{pkcs11.CKR_KEY_FUNCTION_NOT_PERMITTED, CodeKeyAttrs}, {pkcs11.CKR_MECHANISM_INVALID, CodeKeyAttrs},
		{pkcs11.CKR_GENERAL_ERROR, CodeInternal}, // untyped failure: explicit, never swallowed
	} {
		got := Map("sign", pkcs11.Error(c.ckr))
		if got == nil || got.Code != c.want {
			t.Errorf("CKR %#x: %v, want %s", c.ckr, got, c.want)
		}
	}
	if Map("x", errors.New("boom")).Code != CodeInternal {
		t.Error("non-pkcs11 error not mapped to hsm_internal")
	}
	if Map("x", nil) != nil {
		t.Error("nil mapped to an error")
	}
	if !SessionBroken(pkcs11.Error(pkcs11.CKR_SESSION_HANDLE_INVALID)) || SessionBroken(pkcs11.Error(pkcs11.CKR_DEVICE_ERROR)) {
		t.Error("SessionBroken misclassifies")
	}
}

func attr(t uint, v bool) *pkcs11.Attribute {
	b := byte(0)
	if v {
		b = 1
	}
	return pkcs11.NewAttribute(t, []byte{b})
}

func TestCheckAttrs_RefusesAnythingButPrivateSensitiveNonExtractable(t *testing.T) {
	kt := pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, uint(pkcs11.CKK_EC))
	good := func() []*pkcs11.Attribute {
		return []*pkcs11.Attribute{attr(pkcs11.CKA_PRIVATE, true), attr(pkcs11.CKA_SENSITIVE, true), attr(pkcs11.CKA_EXTRACTABLE, false),
			attr(pkcs11.CKA_NEVER_EXTRACTABLE, true), attr(pkcs11.CKA_SIGN, true), kt}
	}
	if err := checkAttrs(good()); err != nil {
		t.Fatalf("good attributes refused: %v", err)
	}
	for name, mut := range map[string]func([]*pkcs11.Attribute){
		"CKA_PRIVATE=false (plaintext in token files)": func(a []*pkcs11.Attribute) { a[0] = attr(pkcs11.CKA_PRIVATE, false) },
		"CKA_PRIVATE missing":                          func(a []*pkcs11.Attribute) { a[0] = pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, nil) },
		"not sensitive":                                func(a []*pkcs11.Attribute) { a[1] = attr(pkcs11.CKA_SENSITIVE, false) },
		"extractable":                                  func(a []*pkcs11.Attribute) { a[2] = attr(pkcs11.CKA_EXTRACTABLE, true) },
		"was once extractable":                         func(a []*pkcs11.Attribute) { a[3] = attr(pkcs11.CKA_NEVER_EXTRACTABLE, false) },
		"cannot sign":                                  func(a []*pkcs11.Attribute) { a[4] = attr(pkcs11.CKA_SIGN, false) },
		"not an EC key":                                func(a []*pkcs11.Attribute) { a[5] = pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, uint(pkcs11.CKK_RSA)) },
	} {
		a := good()
		mut(a)
		err := checkAttrs(a)
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeKeyAttrs {
			t.Errorf("%s: %v, want hsm_key_attributes", name, err)
		}
	}
}

func TestParseECPoint(t *testing.T) {
	pt := append([]byte{0x04}, make([]byte, 64)...)
	if got, err := ParseECPoint(append([]byte{0x04, 0x41}, pt...)); err != nil || len(got) != 65 {
		t.Fatalf("DER-wrapped: %v", err)
	}
	if got, err := ParseECPoint(pt); err != nil || len(got) != 65 {
		t.Fatalf("bare: %v", err)
	}
	if _, err := ParseECPoint(pt[:64]); err == nil {
		t.Fatal("short point accepted")
	}
}

func TestPINGuard_TripsPersistsAndFailsClosedOnCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "pin-guard.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewPINGuard(path, 3, time.Hour)
	g.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if err := g.Allow(); err != nil {
			t.Fatalf("attempt %d refused early: %v", i, err)
		}
		if err := g.RecordFailure(); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Allow(); !errors.Is(err, ErrPINGuardTripped) {
		t.Fatalf("4th attempt: %v, want ErrPINGuardTripped", err)
	}
	// survives a restart (new guard on the same file)
	g2 := NewPINGuard(path, 3, time.Hour)
	g2.now = g.now
	if err := g2.Allow(); !errors.Is(err, ErrPINGuardTripped) {
		t.Fatal("limit did not survive a restart")
	}
	// window passes
	g2.now = func() time.Time { return now.Add(61 * time.Minute) }
	if err := g2.Allow(); err != nil {
		t.Fatalf("still tripped after the window: %v", err)
	}
	// success clears
	_ = g.RecordSuccess()
	if g.Failures() != 0 {
		t.Fatal("success did not clear")
	}
	// corruption must not reset the limit
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if err := g.Allow(); !errors.Is(err, ErrPINGuardTripped) {
		t.Fatal("a corrupted guard file reset the limit")
	}
	var nilGuard *PINGuard
	if nilGuard.Allow() != nil || nilGuard.RecordFailure() != nil {
		t.Fatal("nil guard must be a no-op")
	}
}

func TestReadPIN_FromFileOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pin")
	_ = os.WriteFile(p, []byte("1234 5678\n"), 0o600)
	if pin, err := ReadPIN(p); err != nil || pin != "1234 5678" {
		t.Fatalf("%q %v", pin, err)
	}
	_ = os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := ReadPIN(p); err == nil {
		t.Fatal("empty PIN accepted")
	}
	if _, err := ReadPIN(p + ".missing"); err == nil {
		t.Fatal("missing PIN file accepted")
	}
}

func TestCheckNonce_OnlyExactly96NonZeroBits(t *testing.T) {
	good := bytes.Repeat([]byte{7}, NonceSize)
	if err := CheckNonce(good); err != nil {
		t.Fatal(err)
	}
	last := make([]byte, NonceSize)
	last[NonceSize-1] = 1
	if err := CheckNonce(last); err != nil {
		t.Fatalf("a nonce with one set bit is fine: %v", err)
	}
	for name, n := range map[string][]byte{"nil": nil, "empty": {}, "zero": make([]byte, NonceSize), "short": good[:11], "long": append(append([]byte(nil), good...), 1)} {
		if err := CheckNonce(n); !errors.Is(err, ErrBadNonce) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
