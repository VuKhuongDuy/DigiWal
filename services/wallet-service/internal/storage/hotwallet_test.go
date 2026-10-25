package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"digiwal/services/wallet-service/internal/testdb"
)

const (
	chain = int64(31337)
	hot   = "0xhot"
)

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
			sig, err := s.SignHotWallet(ctx, chain, hot, "sweeper", fmt.Sprintf("k%d", i), "h", seedFn, raw)
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
			sig, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "same", "h", seed(0), raw)
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
	next, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "other", "h", seed(0), raw)
	if err != nil || next.Nonce != 1 {
		t.Fatalf("next nonce = %d (err %v), want 1", next.Nonce, err)
	}
}

func TestSignHotWallet_KeyReuseWithDifferentRequestIsRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "k", "hash-A", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "k", "hash-B", seed(0), raw); !errors.Is(err, ErrIdempotencyKeyReused) {
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
	if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "k1", "h", seed(7), func(uint64) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	sig, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "k2", "h", seed(7), raw)
	if err != nil || sig.Nonce != 7 {
		t.Fatalf("nonce = %d (err %v), want 7: a failed signature must not burn a nonce", sig.Nonce, err)
	}
}

func TestNonceGapFreeze_RefusesNewAllocationButAllowsReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.FreezeNonceGap(ctx, chain, hot, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("freeze before first use: want ErrNotFound, got %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", fmt.Sprintf("k%d", i), "h", seed(0), raw); err != nil {
			t.Fatal(err)
		}
	}
	f, err := s.FreezeNonceGap(ctx, chain, hot, 2)
	if err != nil || f.FromNonce != 2 || f.At.IsZero() {
		t.Fatalf("freeze: %+v %v", f, err)
	}
	if again, err := s.FreezeNonceGap(ctx, chain, hot, 2); err != nil || again != f { // a repeat is a no-op
		t.Fatalf("repeat: %+v %v, want %+v", again, err, f)
	}
	if _, err := s.FreezeNonceGap(ctx, chain, hot, 3); !errors.Is(err, ErrNonceNotAllocated) { // next_nonce is 3
		t.Fatalf("never-allocated nonce: %v, want ErrNonceNotAllocated", err)
	}
	if lower, err := s.FreezeNonceGap(ctx, chain, hot, 1); err != nil || lower.FromNonce != 1 || !lower.At.Equal(f.At) { // only widens
		t.Fatalf("lower: %+v %v", lower, err)
	}
	if again, err := s.FreezeNonceGap(ctx, chain, hot, 2); err != nil || again.FromNonce != 1 { // a higher report never raises it
		t.Fatalf("higher after lower: %+v %v", again, err)
	}
	var from int64
	if err := s.pool.QueryRow(ctx, `SELECT frozen_from_nonce FROM hot_wallet_nonces WHERE chain_id=$1 AND hot_address=$2`, chain, hot).Scan(&from); err != nil || from != 1 {
		t.Fatalf("frozen_from_nonce = %d (err %v), want 1", from, err)
	}

	// new key: refused, whoever asks (invariant #7)
	if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "new", "h", seed(0), raw); !errors.Is(err, ErrNonceGapOpen) {
		t.Fatalf("want ErrNonceGapOpen, got %v", err)
	}
	// already-stored key: replay is fine, it allocates nothing
	if sig, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "k1", "h", seed(0), raw); err != nil || sig.Nonce != 1 {
		t.Fatalf("replay during freeze: %+v %v", sig, err)
	}

	if err := s.ResolveNonceGap(ctx, chain, hot); err != nil {
		t.Fatal(err)
	}
	if sig, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "new", "h", seed(0), raw); err != nil || sig.Nonce != 3 {
		t.Fatalf("after resolve: %+v %v (want nonce 3)", sig, err)
	}
}

func TestNonceStatesReflectFreezeAndResolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if st, err := s.NonceStates(ctx, hot); err != nil || len(st) != 0 {
		t.Fatalf("a chain that never signed has no row: %v %v", st, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO hot_wallet_nonces (chain_id, hot_address, next_nonce) VALUES (31337, '0xhot', 5)`); err != nil {
		t.Fatal(err)
	}
	st, _ := s.NonceStates(ctx, hot)
	if len(st) != 1 || st[0].NextNonce != 5 || st[0].FrozenFrom != nil {
		t.Fatalf("%+v", st)
	}
	if _, err := s.FreezeNonceGap(ctx, 31337, hot, 3); err != nil {
		t.Fatal(err)
	}
	st, _ = s.NonceStates(ctx, hot)
	if st[0].FrozenFrom == nil || *st[0].FrozenFrom != 3 {
		t.Fatalf("%+v", st)
	}
	if err := s.ResolveNonceGap(ctx, 31337, hot); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.NonceStates(ctx, hot); st[0].FrozenFrom != nil {
		t.Fatalf("resolved: %+v", st)
	}
}

// Invariant 24: a new Hot address starts at its own on-chain nonce and never touches another address's counter.
func TestHotNonceStateIsPerAddress(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const other = "0xother"
	for i := 0; i < 3; i++ {
		if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", fmt.Sprintf("old%d", i), "h", seed(10), raw); err != nil {
			t.Fatal(err)
		}
	}
	sig, err := s.SignHotWallet(ctx, chain, other, "sweeper", "new0", "h", seed(0), raw)
	if err != nil || sig.Nonce != 0 {
		t.Fatalf("new address must start at its own nonce 0, got %d err %v", sig.Nonce, err)
	}
	// the old counter is untouched, and both rows coexist for one nonce value
	old, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "old3", "h", seed(99), raw)
	if err != nil || old.Nonce != 13 {
		t.Fatalf("old address nonce = %d (err %v), want 13", old.Nonce, err)
	}
	// a freeze on one address does not stop the other
	if _, err := s.FreezeNonceGap(ctx, chain, hot, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignHotWallet(ctx, chain, hot, "sweeper", "old4", "h", seed(0), raw); !errors.Is(err, ErrNonceGapOpen) {
		t.Fatalf("old address should be frozen, got %v", err)
	}
	if _, err := s.SignHotWallet(ctx, chain, other, "sweeper", "new1", "h", seed(0), raw); err != nil {
		t.Fatalf("new address must not be frozen by the old one's gap: %v", err)
	}
	if st, _ := s.NonceStates(ctx, other); len(st) != 1 || st[0].NextNonce != 2 || st[0].FrozenFrom != nil {
		t.Fatalf("states for the new address = %+v", st)
	}
}

func TestReplaceRefusesAKeySignedByAnotherHotAddress(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, "0xold", "sweeper", "k", "h", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignHotWallet(ctx, chain, "0xnew", "sweeper", "other", "h", seed(0), raw); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReplaceHotKey(ctx, chain, "0xnew", "sweeper", "k", 1, KindFeeBump, func(ReplaceInput) (Built, error) {
		return Built{}, errors.New("must not build")
	})
	if !errors.Is(err, ErrOtherHotKey) {
		t.Fatalf("got %v, want ErrOtherHotKey", err)
	}
}

// Legacy rows (” address) are attributed only when the software backend adopts them; a pkcs11 start never does.
func TestAdoptLegacyHotAddress(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.SignHotWallet(ctx, chain, LegacyHotAddress, "sweeper", "k", "h", seed(4), raw); err != nil {
		t.Fatal(err)
	}
	// a different (new) address does not see the legacy counter
	if sig, err := s.SignHotWallet(ctx, chain, "0xnew", "sweeper", "n", "h", seed(0), raw); err != nil || sig.Nonce != 0 {
		t.Fatalf("new address nonce %d err %v, want 0", sig.Nonce, err)
	}
	if err := s.AdoptLegacyHotAddress(ctx, "0xlegacy"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdoptLegacyHotAddress(ctx, "0xlegacy"); err != nil { // idempotent
		t.Fatal(err)
	}
	sig, err := s.SignHotWallet(ctx, chain, "0xlegacy", "sweeper", "k2", "h", seed(0), raw)
	if err != nil || sig.Nonce != 5 {
		t.Fatalf("adopted address continues its counter: nonce %d err %v, want 5", sig.Nonce, err)
	}
}

func TestMigration000005_BackfillsAndRollsBack(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	m, err := NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(4); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_nonces (chain_id, next_nonce) VALUES (31337, 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, nonce, request_hash, signed_raw_tx) VALUES ('k', 31337, 6, 'h', '\x00')`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(5); err != nil {
		t.Fatalf("up: %v", err)
	}
	var a, b string
	if err := conn.QueryRow(ctx, `SELECT (SELECT hot_address FROM hot_wallet_nonces), (SELECT hot_address FROM hot_wallet_signatures)`).Scan(&a, &b); err != nil || a != "" || b != "" {
		t.Fatalf("existing rows should be legacy (''): %q %q err %v", a, b, err)
	}
	// two addresses may now share a chain and a nonce
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_nonces (chain_id, hot_address, next_nonce) VALUES (31337, '0xa', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, hot_address, nonce, request_hash, signed_raw_tx) VALUES ('k2', 31337, '0xa', 6, 'h', '\x00')`); err != nil {
		t.Fatalf("same nonce, other address must be allowed: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_signatures (idempotency_key, chain_id, hot_address, nonce, request_hash, signed_raw_tx) VALUES ('k3', 31337, '0xa', 6, 'h', '\x00')`); err == nil {
		t.Fatal("same address and nonce must still be unique")
	}
	if err := m.Migrate(4); err == nil {
		t.Fatal("down must refuse while a chain has two Hot addresses")
	}
}

func TestMigration000005_DownRestoresTheOldShape(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	m, err := NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(5); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO hot_wallet_nonces (chain_id, hot_address, next_nonce) VALUES (31337, '0xa', 3)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(4); err != nil {
		t.Fatalf("down with one address per chain: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id = 31337`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("row after down: %d err %v", n, err)
	}
	if _, err := conn.Exec(ctx, `SELECT hot_address FROM hot_wallet_nonces`); err == nil {
		t.Fatal("hot_address column should be gone after down")
	}
}
