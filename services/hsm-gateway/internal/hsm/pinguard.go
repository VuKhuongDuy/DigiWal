package hsm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrPINGuardTripped means too many recent PIN failures are recorded: the gateway refuses to even
// try the token. SoftHSM never locks the token on wrong PINs (M0 spike (e)), so this limit is the
// only online guessing brake, and it also stops a restart loop from hammering a real HSM.
var ErrPINGuardTripped = errors.New("hsm: too many recent PIN failures; refusing to try the token (an operator must investigate, then delete the pin-guard file)")

// PINGuard persists PIN-failure timestamps so the limit survives restarts. A nil *PINGuard allows
// everything (tests only).
type PINGuard struct {
	mu     sync.Mutex
	path   string
	max    int
	window time.Duration
	now    func() time.Time
}

// NewPINGuard allows at most max recorded failures within window.
func NewPINGuard(path string, max int, window time.Duration) *PINGuard {
	return &PINGuard{path: path, max: max, window: window, now: time.Now}
}

type guardState struct {
	Failures []time.Time `json:"failures"`
}

func (g *PINGuard) load() guardState {
	var s guardState
	b, err := os.ReadFile(g.path)
	if err != nil {
		return s
	}
	if json.Unmarshal(b, &s) != nil {
		// Unreadable state is treated as tripped, not as clean: tampering must not reset the limit.
		cut := g.now()
		s.Failures = make([]time.Time, g.max)
		for i := range s.Failures {
			s.Failures[i] = cut
		}
	}
	return s
}

func (g *PINGuard) recent(s guardState) []time.Time {
	var out []time.Time
	for _, t := range s.Failures {
		if g.now().Sub(t) < g.window {
			out = append(out, t)
		}
	}
	return out
}

// Allow returns ErrPINGuardTripped when the recent-failure budget is spent.
func (g *PINGuard) Allow() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := len(g.recent(g.load())); n >= g.max {
		return fmt.Errorf("%w (%d failures within %s)", ErrPINGuardTripped, n, g.window)
	}
	return nil
}

// Failures returns the number of recent recorded failures.
func (g *PINGuard) Failures() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.recent(g.load()))
}

// RecordFailure persists one failed attempt (before the caller exits).
func (g *PINGuard) RecordFailure() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := guardState{Failures: append(g.recent(g.load()), g.now())}
	return g.save(s)
}

// RecordSuccess clears the counter.
func (g *PINGuard) RecordSuccess() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.save(guardState{})
}

func (g *PINGuard) save(s guardState) error {
	b, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(g.path), 0o750); err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}
