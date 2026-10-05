package pipeline

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type spy struct {
	mu        sync.Mutex
	reclaimed []string
	alerted   []string
	passes    []time.Time
}

func (s *spy) Reclaimed(rule string) {
	s.mu.Lock()
	s.reclaimed = append(s.reclaimed, rule)
	s.mu.Unlock()
}
func (s *spy) Alerted(kind string)     { s.mu.Lock(); s.alerted = append(s.alerted, kind); s.mu.Unlock() }
func (s *spy) ReaperPass(at time.Time) { s.mu.Lock(); s.passes = append(s.passes, at); s.mu.Unlock() }

func TestObserverSeesEveryReaperAction(t *testing.T) {
	r := newRig(t)
	r.newRequest("stale-requested")
	r.validated("stale-validated")
	rp, _ := r.reaper(stuck)
	sp := &spy{}
	rp.Observer = sp
	r.reap(rp)
	got := strings.Join(sp.reclaimed, ",")
	if len(sp.reclaimed) != 2 || !strings.Contains(got, ReasonStaleRequest) || !strings.Contains(got, ReasonStaleReservation) {
		t.Fatalf("reclaimed = %v, want one stale_request and one stale_reservation", sp.reclaimed)
	}
	if len(sp.passes) != 1 {
		t.Fatalf("passes = %v", sp.passes)
	}
	// nothing left to do: the pass is still reported (the loop is alive), no action is
	r.reap(rp)
	if len(sp.reclaimed) != 2 || len(sp.passes) != 2 {
		t.Fatalf("an idle pass reports a pass and no action: reclaimed %v passes %d", sp.reclaimed, len(sp.passes))
	}
}

func TestObserverSeesReleasedByAnotherTx(t *testing.T) {
	r := newRig(t)
	r.signedOnly("k")
	r.chain.minedNonce = 1
	rp, _ := r.reaper(0)
	sp := &spy{}
	rp.Observer = sp
	r.reap(rp)
	if len(sp.reclaimed) != 1 || sp.reclaimed[0] != ReasonReplacedOrDropped {
		t.Fatalf("reclaimed = %v", sp.reclaimed)
	}
}

// An ALERT is counted when it is emitted, not when it is suppressed as a repeat.
func TestObserverCountsOnlyAlertsActuallyEmitted(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	rp, alerts := r.reaper(stuck)
	sp := &spy{}
	rp.Observer = sp
	r.reap(rp)
	r.reap(rp)
	r.reap(rp)
	if len(*alerts) != 1 || len(sp.alerted) != 1 || sp.alerted[0] != "reserved-no-void" {
		t.Fatalf("alerts emitted %d, observed %v: want exactly one", len(*alerts), sp.alerted)
	}
}

func TestNilObserverIsFine(t *testing.T) {
	r := newRig(t)
	r.newRequest("old")
	rp, _ := r.reaper(stuck)
	r.reap(rp) // Observer is nil
	if r.get("old").Status != "failed" {
		t.Fatal("the Reaper must work without an observer")
	}
}
