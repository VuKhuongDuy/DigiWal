//go:build !prod

package seedkit

import (
	"testing"

	"digiwal/services/wallet-service/internal/hdwallet"
)

// seedkit duplicates hdwallet's seed functions so key-admin can build with -tags prod; this pins the copy to the original.
func TestSeedkitEqualsHdwallet(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := SeedFromMnemonic(m)
	s2, _ := hdwallet.SeedFromMnemonic(m)
	if string(s1) != string(s2) || len(s1) != 64 {
		t.Fatal("seed differs")
	}
	x1, _ := AccountXPubFromSeed(s1)
	x2, _ := hdwallet.AccountXPubFromSeed(s2)
	if x1 != x2 {
		t.Fatal("xpub differs")
	}
	for i := uint32(0); i < 25; i++ {
		a, _ := AddressAtSeed(s1, i)
		b, _ := hdwallet.AddressAtSeed(s2, i)
		w, _ := hdwallet.FromMnemonic(m)
		c, _ := w.AddressAt(i)
		if a != b || a != c {
			t.Fatalf("index %d: %s %s %s", i, a, b, c)
		}
	}
}
