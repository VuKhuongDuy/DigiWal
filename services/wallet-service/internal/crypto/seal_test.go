//go:build !prod

package crypto

import (
	"encoding/hex"
	"testing"
)

func testKeyHex() string {
	return hex.EncodeToString(make([]byte, 32)) // all-zero key, fine for a test
}

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := NewSealer(testKeyHex())
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("correct horse battery staple")
	sealed, err := s.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) == string(plaintext) {
		t.Fatal("sealed blob must not equal plaintext")
	}

	opened, err := s.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != string(plaintext) {
		t.Fatalf("got %q, want %q", opened, plaintext)
	}
}

func TestOpenFailsWithWrongKey(t *testing.T) {
	s1, _ := NewSealer(testKeyHex())
	wrongKey := hex.EncodeToString(append(make([]byte, 31), 1))
	s2, _ := NewSealer(wrongKey)

	sealed, err := s1.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Open(sealed); err == nil {
		t.Fatal("expected Open with the wrong key to fail, it didn't")
	}
}

func TestNewSealerRejectsBadKeys(t *testing.T) {
	if _, err := NewSealer("not-hex"); err == nil {
		t.Fatal("expected error for non-hex key")
	}
	if _, err := NewSealer(hex.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("expected error for a 16-byte (not 32-byte) key")
	}
}

// Golden blob shared with sweep-signer's vault test: the 64-byte Seed format of key-custody-hsm.md M3b. Key 0x00..01, seed
// bytes 1..64, AAD SeedAAD. If either module changes the AAD or the framing, one of the two tests fails.
func TestSealedSeedGoldenBlobSharedWithSweepSigner(t *testing.T) {
	key := make([]byte, 32)
	key[31] = 1
	s, err := NewSealer(hex.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := hex.DecodeString("0909090909090909090909096789a5782a2bdb75a451506d99b0cccb383da4af8b8b2d2e7df81f81d8b0cd3dae8b3c33a9e4730dced35429f95d3f5846d419aebeccd5236dea3825096cf8674ce5de73f1db4bb59bbb0d94ce828e81")
	want := make([]byte, 64)
	for i := range want {
		want[i] = byte(i + 1)
	}
	got, err := s.OpenAAD(blob, []byte(SeedAAD))
	if err != nil || string(got) != string(want) {
		t.Fatalf("golden blob: %v", err)
	}
	if _, err := s.Open(blob); err == nil {
		t.Fatal("the Seed blob opened with no AAD: the format is not bound to its role")
	}
	if _, err := s.OpenAAD(blob, []byte("hot_wallet|sweep|1")); err == nil {
		t.Fatal("the Seed blob opened under another role's AAD")
	}
	// SealAAD round trip, and a mnemonic-format (no AAD) blob is not a Seed
	sealed, _ := s.SealAAD(want, []byte(SeedAAD))
	if back, err := s.OpenAAD(sealed, []byte(SeedAAD)); err != nil || string(back) != string(want) {
		t.Fatalf("round trip: %v", err)
	}
	legacy, _ := s.Seal(want)
	if _, err := s.OpenAAD(legacy, []byte(SeedAAD)); err == nil {
		t.Fatal("a no-AAD (mnemonic-format) blob authenticated as a Seed")
	}
}
