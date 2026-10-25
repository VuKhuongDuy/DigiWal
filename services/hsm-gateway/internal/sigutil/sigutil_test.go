package sigutil

import (
	"crypto/ecdsa"
	"crypto/rand"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// rawSign emulates what CKM_ECDSA returns: raw r||s from a random-k ECDSA, with no low-s rule.
func rawSign(t testing.TB, priv *ecdsa.PrivateKey, digest []byte) []byte {
	t.Helper()
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

// Property: for >= 10 000 random digests, every raw signature (about half of which have a high s)
// is normalised to low-s and recovers the expected address.
func TestFinalize_PropertyRecoversExpectedAddress(t *testing.T) {
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	want := crypto.PubkeyToAddress(priv.PublicKey)
	const n = 10000
	high, vSeen := 0, map[byte]int{}
	for i := 0; i < n; i++ {
		digest := make([]byte, 32)
		_, _ = rand.Read(digest)
		raw := rawSign(t, priv, digest)
		if new(big.Int).SetBytes(raw[32:]).Cmp(halfN) > 0 {
			high++
		}
		sig, err := Finalize(digest, raw, want)
		if err != nil {
			t.Fatalf("digest %d: %v", i, err)
		}
		if !IsLowS(sig) {
			t.Fatalf("digest %d: s is not low", i)
		}
		pub, err := crypto.SigToPub(digest, sig)
		if err != nil || crypto.PubkeyToAddress(*pub) != want {
			t.Fatalf("digest %d does not recover the expected address", i)
		}
		if !crypto.VerifySignature(crypto.FromECDSAPub(&priv.PublicKey), digest, sig[:64]) {
			t.Fatalf("digest %d: VerifySignature false", i)
		}
		vSeen[sig[64]]++
	}
	if high < n/3 || high > 2*n/3 {
		t.Fatalf("expected roughly half of raw signatures to have a high s, got %d of %d (emulation is off)", high, n)
	}
	if vSeen[0] == 0 || vSeen[1] == 0 {
		t.Fatalf("recovery ids not both exercised: %v", vSeen)
	}
	t.Logf("%d digests, %d raw high-s (%.1f%%), v=0:%d v=1:%d", n, high, 100*float64(high)/n, vSeen[0], vSeen[1])
}

func TestFinalize_Refusals(t *testing.T) {
	priv, _ := crypto.GenerateKey()
	other, _ := crypto.GenerateKey()
	want := crypto.PubkeyToAddress(priv.PublicKey)
	digest := make([]byte, 32)
	_, _ = rand.Read(digest)
	raw := rawSign(t, priv, digest)

	if _, err := Finalize(digest, rawSign(t, other, digest), want); err != ErrNoRecovery {
		t.Fatalf("signature by another key: got %v, want ErrNoRecovery", err)
	}
	if _, err := Finalize(digest, raw[:63], want); err == nil {
		t.Fatal("short signature accepted")
	}
	if _, err := Finalize(digest[:31], raw, want); err == nil {
		t.Fatal("short digest accepted")
	}
	if _, err := Finalize(digest, make([]byte, 64), want); err == nil {
		t.Fatal("all-zero signature accepted")
	}
	flipped := append([]byte(nil), raw...)
	flipped[5] ^= 0xff
	if _, err := Finalize(digest, flipped, want); err == nil {
		t.Fatal("corrupted signature accepted")
	}
}
