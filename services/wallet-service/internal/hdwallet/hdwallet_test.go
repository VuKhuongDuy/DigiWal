package hdwallet

import (
	"regexp"
	"testing"
)

var evmAddressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// A fixed test-only mnemonic (BIP39 test vector) — never used for
// anything real, just to make derivation deterministic in tests.
const testMnemonic = "test test test test test test test test test test test junk"

func TestAddressAtIsDeterministic(t *testing.T) {
	w1, err := FromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := FromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}

	addr1, err := w1.AddressAt(0)
	if err != nil {
		t.Fatal(err)
	}
	addr2, err := w2.AddressAt(0)
	if err != nil {
		t.Fatal(err)
	}

	if addr1 != addr2 {
		t.Fatalf("deriving index 0 twice from the same mnemonic gave different addresses: %s vs %s", addr1, addr2)
	}
	if !evmAddressRE.MatchString(addr1) {
		t.Fatalf("address %q doesn't look like a valid EVM address", addr1)
	}
}

func TestDifferentIndicesGiveDifferentAddresses(t *testing.T) {
	w, err := FromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for i := uint32(0); i < 5; i++ {
		addr, err := w.AddressAt(i)
		if err != nil {
			t.Fatal(err)
		}
		if seen[addr] {
			t.Fatalf("index %d produced an address already seen: %s", i, addr)
		}
		seen[addr] = true
	}
}

func TestNewMnemonicProducesUsableWallet(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	w, err := FromMnemonic(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddressAt(0); err != nil {
		t.Fatal(err)
	}
}
