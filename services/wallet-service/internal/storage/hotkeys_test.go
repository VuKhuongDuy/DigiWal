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
	wd = "withdrawal-service"
	sw = "sweeper"
)

func build(tag string) func(ReplaceInput) (Built, error) {
	return func(in ReplaceInput) (Built, error) {
		return Built{Raw: []byte(fmt.Sprintf("%s-%d-%d", tag, in.Nonce, in.Seq)), Hash: fmt.Sprintf("0x%s%d", tag, in.Seq),
			MaxFee: big.NewInt(int64(100 * in.Seq)), MaxTip: big.NewInt(int64(10 * in.Seq))}, nil
	}
}

func signAs(t *testing.T, s *Store, caller, key string) HotSignature {
	t.Helper()
	sig, err := s.SignHotWallet(context.Background(), chain, caller, key, "h", seed(0), raw)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// invariant #22: a voided key is never signed and allocates no nonce.
func TestVoidThenSignIsRefusedAndAllocatesNothing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	res, err := s.VoidHotKey(ctx, chain, wd, "withdrawal:a", seed(0))
	if err != nil || !res.Voided {
		t.Fatalf("void = %+v, %v", res, err)
	}
	if again, err := s.VoidHotKey(ctx, chain, wd, "withdrawal:a", seed(0)); err != nil || !again.Voided {
		t.Fatalf("a second void = %+v, %v, want voided", again, err)
	}
	if _, err := s.SignHotWallet(ctx, chain, wd, "withdrawal:a", "h", seed(0), raw); !errors.Is(err, ErrKeyVoided) {
		t.Fatalf("sign of a voided key = %v, want ErrKeyVoided", err)
	}
	if next := signAs(t, s, wd, "withdrawal:b"); next.Nonce != 0 {
		t.Fatalf("the refused sign consumed a nonce: next key got %d, want 0", next.Nonce)
	}
}

func TestVoidOfASignedKeyReturnsTheSignature(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sig := signAs(t, s, wd, "withdrawal:a")
	res, err := s.VoidHotKey(ctx, chain, wd, "withdrawal:a", seed(0))
	if err != nil || res.Voided || res.Signed.Nonce != sig.Nonce || string(res.Signed.SignedRawTx) != string(sig.SignedRawTx) {
		t.Fatalf("void of a signed key = %+v, %v", res, err)
	}
	// ... and the signature is untouched: a replay still returns it.
	if again := signAs(t, s, wd, "withdrawal:a"); again.Nonce != sig.Nonce {
		t.Fatal("replay changed")
	}
}

// Void and sign take the same lock: whichever way a race lands, the outcomes agree and the nonces stay dense.
func TestVoidRacingSignNeverBothWin(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("withdrawal:%d", i)
		var wg sync.WaitGroup
		var signErr, voidErr error
		var voided bool
		wg.Add(2)
		go func() { defer wg.Done(); _, signErr = s.SignHotWallet(ctx, chain, wd, key, "h", seed(0), raw) }()
		go func() {
			defer wg.Done()
			var r VoidResult
			r, voidErr = s.VoidHotKey(ctx, chain, wd, key, seed(0))
			voided = r.Voided
		}()
		wg.Wait()
		if voidErr != nil {
			t.Fatal(voidErr)
		}
		if voided != errors.Is(signErr, ErrKeyVoided) || (signErr != nil && !voided) {
			t.Fatalf("%s: voided=%v but sign err=%v", key, voided, signErr)
		}
	}
	// 20 keys, whatever mix was signed: the next nonce equals the number signed (no gap, no hole).
	var next, signed int64
	_ = s.pool.QueryRow(ctx, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id = $1`, chain).Scan(&next)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM hot_wallet_signatures`).Scan(&signed)
	if next != signed {
		t.Fatalf("next_nonce %d != %d signatures: a nonce was allocated and not used", next, signed)
	}
}

func TestKeysAreScopedToTheirCaller(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	signAs(t, s, wd, "withdrawal:a")
	if _, err := s.GetHotKey(ctx, sw, "withdrawal:a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another caller's GET = %v, want ErrNotFound", err)
	}
	if _, err := s.GetHotKey(ctx, wd, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if k, err := s.GetHotKey(ctx, wd, "withdrawal:a"); err != nil || k.Nonce != 0 {
		t.Fatalf("own GET = %+v, %v", k, err)
	}
	if _, err := s.VoidHotKey(ctx, chain, sw, "withdrawal:a", seed(0)); !errors.Is(err, ErrNotCallers) {
		t.Fatalf("another caller's void = %v, want ErrNotCallers", err)
	}
	if _, err := s.ReplaceHotKey(ctx, chain, sw, "withdrawal:a", 1, KindFeeBump, build("x")); !errors.Is(err, ErrNotCallers) {
		t.Fatalf("another caller's replace = %v, want ErrNotCallers", err)
	}
	if _, err := s.SignHotWallet(ctx, chain, sw, "withdrawal:a", "h", seed(0), raw); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("another caller replaying the key = %v", err)
	}
	// A tombstone is scoped too.
	if _, err := s.VoidHotKey(ctx, chain, wd, "withdrawal:v", seed(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VoidHotKey(ctx, chain, sw, "withdrawal:v", seed(0)); !errors.Is(err, ErrNotCallers) {
		t.Fatalf("voiding another caller's tombstone = %v", err)
	}
}

// Rows signed before the migration have caller_cn NULL: not anyone's for GET/void/replace, but the
// sender replaying its own key still gets its signature back.
func TestLegacyRowsAreNobodysButStillReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	signAs(t, s, wd, "withdrawal:old")
	if _, err := s.pool.Exec(ctx, `UPDATE hot_wallet_signatures SET caller_cn = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetHotKey(ctx, wd, "withdrawal:old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GET of a legacy row = %v", err)
	}
	if _, err := s.VoidHotKey(ctx, chain, wd, "withdrawal:old", seed(0)); !errors.Is(err, ErrNotCallers) {
		t.Fatalf("void of a legacy row = %v", err)
	}
	if sig := signAs(t, s, wd, "withdrawal:old"); sig.Nonce != 0 {
		t.Fatal("a legacy replay changed")
	}
}

func TestReplacementSequence(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "nope", 1, KindFeeBump, build("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace of an unsigned key = %v", err)
	}
	signAs(t, s, wd, "withdrawal:a")
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 2, KindFeeBump, build("x")); !errors.Is(err, ErrReplacementGap) {
		t.Fatalf("seq 2 before 1 = %v", err)
	}
	r1, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 1, KindFeeBump, build("a"))
	if err != nil || r1.Seq != 1 || r1.Nonce != 0 || string(r1.SignedRawTx) != "a-0-1" {
		t.Fatalf("%+v %v", r1, err)
	}
	// The same seq replays the stored one; the builder is not asked again.
	again, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 1, KindFeeBump, func(ReplaceInput) (Built, error) { return Built{}, errors.New("must not rebuild") })
	if err != nil || string(again.SignedRawTx) != "a-0-1" {
		t.Fatalf("replay = %+v %v", again, err)
	}
	r2, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 2, KindFeeBump, func(in ReplaceInput) (Built, error) {
		if string(in.LatestRaw) != "a-0-1" || in.LatestKind != KindFeeBump || string(in.OriginalRaw) != "tx-0" {
			t.Errorf("builder input = %+v", in)
		}
		return build("a")(in)
	})
	if err != nil || r2.Seq != 2 {
		t.Fatalf("%+v %v", r2, err)
	}
	if next := signAs(t, s, wd, "withdrawal:b"); next.Nonce != 1 {
		t.Fatalf("a replacement consumed a nonce: next got %d, want 1", next.Nonce)
	}
	k, err := s.GetHotKey(ctx, wd, "withdrawal:a")
	if err != nil || len(k.Replacements) != 2 || k.Replacements[1].MaxFeePerGas.Int64() != 200 {
		t.Fatalf("%+v %v", k, err)
	}
}

func TestReplaceUnderAFreezeAndCancel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	signAs(t, s, wd, "withdrawal:a")
	signAs(t, s, wd, "withdrawal:b") // nonce 1
	if _, err := s.FreezeNonceGap(ctx, chain, 1); err != nil {
		t.Fatal(err)
	}
	// nonce 0 is below the freeze: still replaceable. nonce 1 is covered: refused for a caller...
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 1, KindFeeBump, build("a")); err != nil {
		t.Fatalf("replace below the freeze: %v", err)
	}
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:b", 1, KindFeeBump, build("b")); !errors.Is(err, ErrNonceGapOpen) {
		t.Fatalf("replace under a covering freeze = %v, want ErrNonceGapOpen", err)
	}
	// ... but the operator's Nonce Filler is how a gap is repaired.
	c, err := s.ReplaceHotKey(ctx, chain, "", "withdrawal:b", 0, KindCancel, build("c"))
	if err != nil || c.Kind != KindCancel || c.Seq != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	// Retrying the command returns the same filler; a Fee Bump cannot undo it.
	if again, err := s.ReplaceHotKey(ctx, chain, "", "withdrawal:b", 0, KindCancel, build("zzz")); err != nil || string(again.SignedRawTx) != string(c.SignedRawTx) {
		t.Fatalf("cancel retry = %+v %v", again, err)
	}
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 2, KindFeeBump, build("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceHotKey(ctx, chain, "", "withdrawal:a", 0, KindCancel, build("c")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceHotKey(ctx, chain, wd, "withdrawal:a", 4, KindFeeBump, build("a")); !errors.Is(err, ErrKeyCancelled) {
		t.Fatalf("fee bump after a filler = %v, want ErrKeyCancelled", err)
	}
}
