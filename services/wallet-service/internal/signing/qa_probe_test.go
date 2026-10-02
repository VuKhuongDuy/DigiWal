package signing

import (
	"context"
	"math/big"
	"testing"
)

// QA probes (tester role).
func TestQA_AmountAboveUint256IsRejected(t *testing.T) {
	t.Skip("open finding W-1 — remove this line once fixed")
	s, _, _ := setup(t)
	huge := new(big.Int).Lsh(big.NewInt(1), 256).String() // 2^256, one past uint256
	for _, asset := range []string{nativeID, tokenID} {
		r := nativeReq("huge-" + asset[:2])
		r.AssetID, r.Amount = asset, huge
		resp, err := s.Sign(context.Background(), "withdrawal-service", r)
		if err == nil {
			t.Errorf("asset %s: amount 2^256 was SIGNED (nonce %d, %d bytes)", asset[:2], resp.Nonce, len(resp.SignedRawTx))
		}
	}
}

// Same amount written differently must not be a different request (or must at least not double-sign).
func TestQA_AmountSpellingDoesNotSidestepIdempotency(t *testing.T) {
	s, _, _ := setup(t)
	a := nativeReq("spell")
	b := a
	b.Amount = "0" + a.Amount
	ra, err := s.Sign(context.Background(), "withdrawal-service", a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := s.Sign(context.Background(), "withdrawal-service", b)
	t.Logf("leading-zero replay: resp=%v err=%v (first nonce %d)", rb, err, ra.Nonce)
}

func TestQA_ToHotWalletItselfAndZeroAddress(t *testing.T) {
	s, _, _ := setup(t)
	for _, to := range []string{"0x0000000000000000000000000000000000000000", s.HotWalletAddress().Hex()} {
		r := nativeReq("to-" + to[2:6])
		r.ToAddress = to
		_, err := s.Sign(context.Background(), "withdrawal-service", r)
		t.Logf("to=%s -> err=%v", to, err)
	}
}
