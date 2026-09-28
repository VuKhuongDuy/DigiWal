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
