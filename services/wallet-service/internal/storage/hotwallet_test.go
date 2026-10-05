package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"digiwal/services/wallet-service/internal/testdb"
)

const chain = int64(31337)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func seed(n uint64) func(context.Context) (uint64, error) {
	return func(context.Context) (uint64, error) { return n, nil }
}

// raw "signs" by embedding the nonce, so replays are verifiably the original.
func raw(nonce uint64) ([]byte, error) { return []byte(fmt.Sprintf("tx-%d", nonce)), nil }

func TestSignHotWallet_ConcurrentDistinctKeysGetDenseUniqueNonces(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	var seeds atomic.Int32
	seedFn := func(context.Context) (uint64, error) { seeds.Add(1); return 5, nil }

	const n = 60
	nonces := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sig, err := s.SignHotWallet(ctx, chain, "sweeper", fmt.Sprintf("k%d", i), "h", seedFn, raw)
			if err != nil {
				t.Error(err)
				return
			}
			nonces[i] = sig.Nonce
		}()
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for _, n := range nonces {
		if seen[n] {
			t.Fatalf("nonce %d issued twice", n)
		}
		seen[n] = true
	}
	for want := uint64(5); want < 5+n; want++ { // starts at the seeded value, no gaps
		if !seen[want] {
			t.Fatalf("nonce %d never issued: nonces must be dense from the seed", want)
		}
	}
	if seeds.Load() < 1 {
		t.Fatal("seed never called")
	}
}

func TestSignHotWallet_SameKeyConcurrentlyClaimsOneNonce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 20
	got := make([]HotSignature, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sig, err := s.SignHotWallet(ctx, chain, "sweeper", "same", "h", seed(0), raw)
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = sig
		}()
	}
	wg.Wait()
	for _, sig := range got {
		if sig.Nonce != 0 || string(sig.SignedRawTx) != "tx-0" {
			t.Fatalf("retries must all see the first result, got %+v", sig)
		}
	}
	// and the next key gets nonce 1, i.e. exactly one nonce was consumed
	next, err := s.SignHotWallet(ctx, chain, "sweeper", "other", "h", seed(0), raw)
	if err != nil || next.Nonce != 1 {
		t.Fatalf("next nonce = %d (err %v), want 1", next.Nonce, err)
	}
}

func TestSignHotWallet_KeyReuseWithDifferentRequestIsRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, "sweeper", "k", "hash-A", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignHotWallet(ctx, chain, "sweeper", "k", "hash-B", seed(0), raw); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("want ErrIdempotencyKeyReused, got %v", err)
	}
	if _, err := s.LookupHotSignature(ctx, "sweeper", "k", "hash-B"); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("lookup: want ErrIdempotencyKeyReused, got %v", err)
	}
	if _, err := s.LookupHotSignature(ctx, "sweeper", "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup missing: want ErrNotFound, got %v", err)
	}
}

func TestSignHotWallet_FailedSigningConsumesNoNonce(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	boom := errors.New("boom")
	if _, err := s.SignHotWallet(ctx, chain, "sweeper", "k1", "h", seed(7), func(uint64) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	sig, err := s.SignHotWallet(ctx, chain, "sweeper", "k2", "h", seed(7), raw)
	if err != nil || sig.Nonce != 7 {
		t.Fatalf("nonce = %d (err %v), want 7: a failed signature must not burn a nonce", sig.Nonce, err)
	}
}

func TestNonceGapFreeze_RefusesNewAllocationButAllowsReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.FreezeNonceGap(ctx, chain, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("freeze before first use: want ErrNotFound, got %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.SignHotWallet(ctx, chain, "sweeper", fmt.Sprintf("k%d", i), "h", seed(0), raw); err != nil {
			t.Fatal(err)
		}
	}
	f, err := s.FreezeNonceGap(ctx, chain, 2)
	if err != nil || f.FromNonce != 2 || f.At.IsZero() {
		t.Fatalf("freeze: %+v %v", f, err)
	}
	if again, err := s.FreezeNonceGap(ctx, chain, 2); err != nil || again != f { // a repeat is a no-op
		t.Fatalf("repeat: %+v %v, want %+v", again, err, f)
	}
	if _, err := s.FreezeNonceGap(ctx, chain, 3); !errors.Is(err, ErrNonceNotAllocated) { // next_nonce is 3
		t.Fatalf("never-allocated nonce: %v, want ErrNonceNotAllocated", err)
	}
	if lower, err := s.FreezeNonceGap(ctx, chain, 1); err != nil || lower.FromNonce != 1 || !lower.At.Equal(f.At) { // only widens
		t.Fatalf("lower: %+v %v", lower, err)
	}
	if again, err := s.FreezeNonceGap(ctx, chain, 2); err != nil || again.FromNonce != 1 { // a higher report never raises it
		t.Fatalf("higher after lower: %+v %v", again, err)
	}
	var from int64
	if err := s.pool.QueryRow(ctx, `SELECT frozen_from_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chain).Scan(&from); err != nil || from != 1 {
		t.Fatalf("frozen_from_nonce = %d (err %v), want 1", from, err)
	}

	// new key: refused, whoever asks (invariant #7)
	if _, err := s.SignHotWallet(ctx, chain, "sweeper", "new", "h", seed(0), raw); !errors.Is(err, ErrNonceGapOpen) {
		t.Fatalf("want ErrNonceGapOpen, got %v", err)
	}
	// already-stored key: replay is fine, it allocates nothing
	if sig, err := s.SignHotWallet(ctx, chain, "sweeper", "k1", "h", seed(0), raw); err != nil || sig.Nonce != 1 {
		t.Fatalf("replay during freeze: %+v %v", sig, err)
	}

	if err := s.ResolveNonceGap(ctx, chain); err != nil {
		t.Fatal(err)
	}
	if sig, err := s.SignHotWallet(ctx, chain, "sweeper", "new", "h", seed(0), raw); err != nil || sig.Nonce != 3 {
		t.Fatalf("after resolve: %+v %v (want nonce 3)", sig, err)
	}
}

func TestNonceStatesReflectFreezeAndResolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if st, err := s.NonceStates(ctx); err != nil || len(st) != 0 {
		t.Fatalf("a chain that never signed has no row: %v %v", st, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO hot_wallet_nonces (chain_id, next_nonce) VALUES (31337, 5)`); err != nil {
		t.Fatal(err)
	}
	st, _ := s.NonceStates(ctx)
	if len(st) != 1 || st[0].NextNonce != 5 || st[0].FrozenFrom != nil {
		t.Fatalf("%+v", st)
	}
	if _, err := s.FreezeNonceGap(ctx, 31337, 3); err != nil {
		t.Fatal(err)
	}
	st, _ = s.NonceStates(ctx)
	if st[0].FrozenFrom == nil || *st[0].FrozenFrom != 3 {
		t.Fatalf("%+v", st)
	}
	if err := s.ResolveNonceGap(ctx, 31337); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.NonceStates(ctx); st[0].FrozenFrom != nil {
		t.Fatalf("resolved: %+v", st)
	}
}
