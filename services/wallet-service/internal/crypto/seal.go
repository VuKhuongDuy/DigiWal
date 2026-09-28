// Package crypto seals the one secret this whole system cannot afford
// to leak: the master HD wallet seed. AES-256-GCM here stands in for a
// real KMS/HSM — see docs/ARCHITECTURE.md's security notes.
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
// 64-character hex string (env var MASTER_KEY_HEX).
func NewSealer(masterKeyHex string) (*Sealer, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("MASTER_KEY_HEX is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("MASTER_KEY_HEX must decode to 32 bytes (got %d) — generate one with: openssl rand -hex 32", len(key))
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
