package sweep

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"digiwal/services/sweeper/internal/ledger"
	"digiwal/services/sweeper/internal/wallet"
)

type spyObs struct {
	mu        sync.Mutex
	started   []string
	ended     []string // "kind:ok" / "kind:failed"
	bumps     []string
	alerts    []string
	abandoned int
}

func (o *spyObs) Start(kind string) func(bool) {
	o.mu.Lock()
	o.started = append(o.started, kind)
	o.mu.Unlock()
	return func(ok bool) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.ended = append(o.ended, fmt.Sprintf("%s:%v", kind, ok))
	}
}
func (o *spyObs) FeeBump(k string) { o.mu.Lock(); o.bumps = append(o.bumps, k); o.mu.Unlock() }
func (o *spyObs) Alerted(k string) { o.mu.Lock(); o.alerts = append(o.alerts, k); o.mu.Unlock() }
func (o *spyObs) Abandoned()       { o.mu.Lock(); o.abandoned++; o.mu.Unlock() }

func (o *spyObs) join(v []string) string { return strings.Join(v, ",") }

func TestObserverSeesATopUpAndASweepStartAndFinish(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	s := newSweeper(w)
	spy := &spyObs{}
	s.Observer = spy
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 3)))
	if spy.join(spy.started) != "gas_topup,sweep" || spy.join(spy.ended) != "gas_topup:true,sweep:true" {
		t.Fatalf("started %v ended %v", spy.started, spy.ended)
	}
	if len(spy.alerts) != 0 || len(spy.bumps) != 0 {
		t.Fatalf("a clean run raises nothing: %v %v", spy.alerts, spy.bumps)
	}
}

// A top-up that never mines ends as a failure: it leaves the in-flight set and is not a completion time.
func TestObserverSeesAFeeBumpAndAnExhaustedTopUp(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true
	s, _, _ := bumpSweeper(w)
	s.Config.InclusionTimeout = 600 * time.Millisecond
	spy := &spyObs{}
	s.Observer = spy
	_ = s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0))
	if len(spy.bumps) != 5 || spy.bumps[0] != "gas_topup" {
		t.Fatalf("bumps = %v", spy.bumps)
	}
	if spy.join(spy.alerts) != "bump_exhausted" {
		t.Fatalf("alerts = %v", spy.alerts)
	}
	if spy.join(spy.ended) != "gas_topup:false" {
		t.Fatalf("ended = %v", spy.ended)
	}
}

func TestObserverSeesARefusedBump(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true
	w.bumpErr = fmt.Errorf("%w: fee ceiling", wallet.ErrPermanent)
	s, _, _ := bumpSweeper(w)
	s.Config.InclusionTimeout = 400 * time.Millisecond
	spy := &spyObs{}
	s.Observer = spy
	_ = s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0))
	if spy.join(spy.alerts) != "bump_refused" || len(spy.bumps) != 0 {
		t.Fatalf("alerts %v bumps %v", spy.alerts, spy.bumps)
	}
}

func TestObserverSeesGasFeeReportFailures(t *testing.T) {
	for name, errs := range map[string][]error{
		"refused":          {fmt.Errorf("%w (409): different values", ledger.ErrRefused)},
		"three transients": {errors.New("503"), errors.New("503"), errors.New("503")},
	} {
		w := newWorld()
		w.native[user] = eth(5)
		s, _, led := bumpSweeper(w)
		w.headStep = 0
		led.errs = errs
		spy := &spyObs{}
		s.Observer = spy
		ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
		if spy.join(spy.alerts) != "gas_fee_report_failed" {
			t.Errorf("%s: alerts = %v", name, spy.alerts)
		}
	}
}

func TestObserverSeesAbandonedRecords(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	w.userErr = errors.New("wallet-service down")
	s := newSweeper(w)
	spy := &spyObs{}
	s.Observer = spy
	rd, stop := runFor(t, s, []Message{msgFor(t, deposit(nativeAsset, eth(5), -1))}, 2, func() bool { return true })
	deadline := time.Now().Add(15 * time.Second)
	for rd.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if spy.abandoned != 1 {
		t.Fatalf("abandoned = %d, want 1 (gave up after max attempts)", spy.abandoned)
	}

	// a record that can never succeed is abandoned at once
	w2 := newWorld()
	s2 := newSweeper(w2)
	spy2 := &spyObs{}
	s2.Observer = spy2
	bad := Message{Topic: "deposits.confirmed", Key: []byte("k"), Value: []byte(`nonsense`)}
	rd2, stop2 := runFor(t, s2, []Message{bad}, 5, func() bool { return true })
	for rd2.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop2()
	if spy2.abandoned != 1 {
		t.Fatalf("abandoned = %d, want 1 (permanent)", spy2.abandoned)
	}
}

func TestNilObserverIsFine(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	s := newSweeper(w) // Observer nil
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 3)))
}
