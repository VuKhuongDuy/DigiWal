package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
)

const (
	oldHot = "0xold0000000000000000000000000000000000001"
	newHot = "0xnew0000000000000000000000000000000000002"
)

func TestFreezeStopsNewSignsButNotReplays(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, oldHot, "withdrawal-service", "before", "h", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	c, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot)
	if err != nil || c.State != CutoverFrozen {
		t.Fatalf("freeze: %+v %v", c, err)
	}
	if _, err := s.SignHotWallet(ctx, chain, oldHot, "withdrawal-service", "after", "h", seed(0), raw); !errors.Is(err, ErrHotCutover) {
		t.Fatalf("a new key while frozen: %v, want ErrHotCutover", err)
	}
	// The freeze is on the chain, not the address: the new Hot address cannot sign either.
	if _, err := s.SignHotWallet(ctx, chain, newHot, "withdrawal-service", "after-new", "h", seed(0), raw); !errors.Is(err, ErrHotCutover) {
		t.Fatalf("the new address while frozen: %v, want ErrHotCutover", err)
	}
	// A key signed before the freeze still replays, with the same nonce.
	sig, err := s.SignHotWallet(ctx, chain, oldHot, "withdrawal-service", "before", "h", seed(0), raw)
	if err != nil || sig.Nonce != 0 {
		t.Fatalf("replay while frozen: %+v %v", sig, err)
	}
	// Nothing was consumed by the refused signs.
	if next, _, ok, _ := s.NonceRow(ctx, chain, oldHot); !ok || next != 1 {
		t.Fatalf("next_nonce = %d, want 1", next)
	}
}

func TestFreezeIsIdempotentAndOneOpenPerChain(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot)
	if err != nil || !a.FrozenAt.Equal(b.FrozenAt) {
		t.Fatalf("a repeat must return the same row: %+v %v", b, err)
	}
	if _, err := s.FreezeHotSigning(ctx, chain, oldHot, "0xother"); !errors.Is(err, ErrCutoverMismatch) {
		t.Fatalf("a different new address for the same cutover: %v", err)
	}
	if _, err := s.FreezeHotSigning(ctx, chain, "0xanotherold", newHot); !errors.Is(err, ErrCutoverOpen) {
		t.Fatalf("a second open cutover on the chain: %v", err)
	}
}

func TestFreezeWaitsForAnInFlightSign(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, oldHot, "c", "seed", "h", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	inSign, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A sign that holds the nonce-row lock while "the HSM" answers.
		_, err := s.SignHotWallet(ctx, chain, oldHot, "c", "slow", "h", seed(0), func(n uint64) ([]byte, error) {
			close(inSign)
			<-release
			return raw(n)
		})
		if err != nil {
			t.Errorf("the in-flight sign must complete: %v", err)
		}
	}()
	<-inSign
	frozen := make(chan error, 1)
	go func() { _, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot); frozen <- err }()
	select {
	case err := <-frozen:
		t.Fatalf("the freeze did not wait for the in-flight sign (%v)", err)
	default:
	}
	close(release)
	wg.Wait()
	if err := <-frozen; err != nil {
		t.Fatal(err)
	}
	// The sign that was in flight is in next_nonce before the freeze is visible.
	if next, _, _, _ := s.NonceRow(ctx, chain, oldHot); next != 2 {
		t.Fatalf("next_nonce = %d, want 2", next)
	}
}

func TestAbortOnlyBeforeThePointOfNoReturn(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.AbortCutover(ctx, chain, oldHot); err != nil {
		t.Fatalf("abort with nothing open is a no-op: %v", err)
	}
	if _, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortCutover(ctx, chain, oldHot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignHotWallet(ctx, chain, oldHot, "c", "k", "h", seed(0), raw); err != nil {
		t.Fatalf("signing after an abort: %v", err)
	}
	// Plan a drain: now there is no way back.
	if _, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot); err != nil {
		t.Fatal(err)
	}
	if planned, err := s.PlanDrain(ctx, chain, oldHot, seed(1), planOne(1)); err != nil || !planned {
		t.Fatalf("plan: %v %v", planned, err)
	}
	if err := s.AbortCutover(ctx, chain, oldHot); !errors.Is(err, ErrCutoverState) {
		t.Fatalf("abort after the point of no return: %v", err)
	}
}

func planOne(round int) func(uint64) ([]DrainPlan, error) {
	return func(first uint64) ([]DrainPlan, error) {
		return []DrainPlan{{Key: DrainKey(chain, oldHot, "asset-a", round), AssetID: "asset-a", Round: round, Amount: big.NewInt(7),
			Raw: []byte(fmt.Sprintf("drain-%d", first)), Hash: fmt.Sprintf("0xhash%d", first)}}, nil
	}
}

func TestPlanDrainIsAtomicIdempotentAndKeepsTheSequenceDense(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, oldHot, "c", "w1", "h", seed(4), raw); err != nil { // nonce 4
		t.Fatal(err)
	}
	if _, err := s.PlanDrain(ctx, chain, oldHot, seed(0), planOne(1)); !errors.Is(err, ErrCutoverState) {
		t.Fatalf("plan without a freeze: %v", err)
	}
	if _, err := s.FreezeHotSigning(ctx, chain, oldHot, newHot); err != nil {
		t.Fatal(err)
	}

	// A build error stores nothing.
	if _, err := s.PlanDrain(ctx, chain, oldHot, seed(0), func(uint64) ([]DrainPlan, error) { return nil, errors.New("sign failed") }); err == nil {
		t.Fatal("expected the build error")
	}
	if ds, _ := s.Drains(ctx, chain, oldHot); len(ds) != 0 {
		t.Fatalf("a failed plan left %d drains", len(ds))
	}
	if c, _ := s.GetCutover(ctx, chain, oldHot); c.State != CutoverFrozen {
		t.Fatalf("a failed plan moved the state to %s", c.State)
	}

	planned, err := s.PlanDrain(ctx, chain, oldHot, seed(0), planOne(1))
	if err != nil || !planned {
		t.Fatalf("plan: %v %v", planned, err)
	}
	ds, err := s.Drains(ctx, chain, oldHot)
	if err != nil || len(ds) != 1 || ds[0].Nonce != 5 || ds[0].Amount.Int64() != 7 || string(ds[0].Raw) != "drain-5" {
		t.Fatalf("drains: %+v %v", ds, err)
	}
	if next, _, _, _ := s.NonceRow(ctx, chain, oldHot); next != 6 {
		t.Fatalf("next_nonce = %d, want 6 (the drain consumed nonce 5)", next)
	}
	if c, _ := s.GetCutover(ctx, chain, oldHot); c.State != CutoverDraining || c.PonrAt == nil {
		t.Fatalf("state %s ponr %v", c.State, c.PonrAt)
	}
	// The drain's signature is a normal hot_wallet_signatures row of the old address: one dense, unique sequence.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM hot_wallet_signatures WHERE hot_address = $1 AND caller_cn = 'hot-cutover' AND nonce = 5`, oldHot).Scan(&n); err != nil || n != 1 {
		t.Fatalf("drain signature row: %d %v", n, err)
	}

	// Planning the same round again is a replay: nothing changes.
	planned, err = s.PlanDrain(ctx, chain, oldHot, seed(0), planOne(1))
	if err != nil || planned {
		t.Fatalf("a repeat must be a no-op: %v %v", planned, err)
	}
	if next, _, _, _ := s.NonceRow(ctx, chain, oldHot); next != 6 {
		t.Fatalf("a repeat advanced next_nonce to %d", next)
	}

	// Progress marks are idempotent.
	for i := 0; i < 2; i++ {
		if err := s.MarkDrainBroadcast(ctx, ds[0].Key); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDrainConfirmed(ctx, ds[0].Key, 21000, big.NewInt(9)); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDrainGasPosted(ctx, ds[0].Key); err != nil {
			t.Fatal(err)
		}
	}
	ds, _ = s.Drains(ctx, chain, oldHot)
	if ds[0].ConfirmedAt == nil || ds[0].GasPostedAt == nil || ds[0].GasUsed != 21000 || ds[0].EffectiveGasPrice.Int64() != 9 {
		t.Fatalf("progress not recorded: %+v", ds[0])
	}

	// State machine: draining -> drained -> done, repeatable; nothing else.
	if err := s.SetCutoverState(ctx, chain, oldHot, CutoverDone); !errors.Is(err, ErrCutoverState) {
		t.Fatalf("draining -> done must be refused: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.SetCutoverState(ctx, chain, oldHot, CutoverDrained); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.SetCutoverState(ctx, chain, oldHot, CutoverDone); err != nil {
			t.Fatal(err)
		}
	}
	// Done unfreezes, and the old address's counter did not move.
	if _, err := s.SignHotWallet(ctx, chain, newHot, "c", "after", "h", seed(0), raw); err != nil {
		t.Fatalf("signing after the cutover is done: %v", err)
	}
	if next, _, _, _ := s.NonceRow(ctx, chain, newHot); next != 1 {
		t.Fatalf("the new address starts at its own nonce, got next_nonce %d", next)
	}
	if next, _, _, _ := s.NonceRow(ctx, chain, oldHot); next != 6 {
		t.Fatalf("old next_nonce = %d", next)
	}
}

func TestHotTxsFromListsVersionsLatestLast(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.SignHotWallet(ctx, chain, oldHot, "withdrawal-service", fmt.Sprintf("k%d", i), "h", seed(10), raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReplaceHotKey(ctx, chain, oldHot, "", "k1", 0, KindFeeBump, func(in ReplaceInput) (Built, error) {
		return Built{Raw: []byte("bumped"), Hash: "0xb", MaxFee: big.NewInt(2), MaxTip: big.NewInt(1)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	txs, err := s.HotTxsFrom(ctx, chain, oldHot, 11)
	if err != nil || len(txs) != 2 || txs[0].Nonce != 11 || len(txs[0].Versions) != 2 || string(txs[0].Versions[1]) != "bumped" || txs[1].Nonce != 12 {
		t.Fatalf("%+v %v", txs, err)
	}
}
