//go:build !prod

// Package crypto is the LEGACY software sealer (dev/e2e regression tooling only, compiled out of a `-tags prod` build): AES-256-GCM under a
// 32-byte software key. Since key-custody-hsm.md M3c nothing in production holds such a key: the user seed is sealed by hsm-gateway under a KEK in the
// HSM. Only the software Hot Wallet of the e2e baseline, key-admin's legacy commands and the Hot cutover tool still use this.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

type Sealer struct {
	gcm cipher.AEAD
}

// NewSealer builds a Sealer from a 32-byte AES-256 key given as a
// 64-character hex string (the legacy software key).
func NewSealer(masterKeyHex string) (*Sealer, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("the legacy software key is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("the legacy software key must decode to 32 bytes (got %d)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{gcm: gcm}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext.
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a blob produced by Seal.
func (s *Sealer) Open(blob []byte) ([]byte, error) {
	n := s.gcm.NonceSize()
	if len(blob) < n {
		return nil, errors.New("sealed blob too short")
	}
	nonce, ciphertext := blob[:n], blob[n:]
	return s.gcm.Open(nil, nonce, ciphertext, nil)
}

// SeedAAD is the associated data of a sealed 64-byte Seed (key-custody-hsm.md M3b): role | purpose | format version. It binds
// the blob so that a row of another role, or a mnemonic-format blob (sealed with no AAD), does not authenticate as a Seed.
// sweep-signer's vault package carries the same string; a golden blob in both test suites pins them to each other.
const SeedAAD = "user_derivation|sweep|1"

// SealAAD encrypts plaintext binding aad, returning nonce||ciphertext.
func (s *Sealer) SealAAD(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// OpenAAD decrypts a blob produced by SealAAD with the same aad.
func (s *Sealer) OpenAAD(blob, aad []byte) ([]byte, error) {
	n := s.gcm.NonceSize()
	if len(blob) < n {
		return nil, errors.New("sealed blob too short")
	}
	return s.gcm.Open(nil, blob[:n], blob[n:], aad)
}
