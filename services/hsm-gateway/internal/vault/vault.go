// Package vault is the wire format and the rules of the Sealed Secret operations of hsm-gateway (key-custody-hsm.md
// invariants 17-20, M3c): which (role, purpose) pairs exist, who may open each, how the AAD is built, and strict decoding of the
// request bodies. The gateway rebuilds the AAD from the request fields itself and compares it with the stored one, so a row
// presented under another role, purpose, version or KEK fails authentication instead of being trusted on the caller's word.
package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Roles and purposes.
const (
	RoleUserDerivation = "user_derivation"
	PurposeSweep       = "sweep"
)

// ErrShape is returned for any request that is not exactly the allowed shape. Nothing was done.
var ErrShape = errors.New("vault: refused request shape")

// Spec says what a role may be sealed for, how large its plaintext is, and which caller CN may unseal it.
type Spec struct {
	Purpose  string
	PlainLen int      // exact plaintext length
	Unseal   []string // CNs allowed to POST /v1/vault/unseal for this role
}

// DefaultSpecs is the production table: one Sealed Secret exists, the 64-byte user_derivation Seed, opened for Sweeps by sweep-signer only.
// Hardcoded on purpose (like the endpoint allow-lists): adding a role is a reviewed code change.
func DefaultSpecs() map[string]Spec {
	return map[string]Spec{RoleUserDerivation: {Purpose: PurposeSweep, PlainLen: 64, Unseal: []string{"sweep-signer"}}}
}

// AAD is the associated data bound by the GCM tag: role | purpose | version | kek_version | SHA256(wrapped_dek) in hex. Binding the
// hash of the wrapped DEK ties the ciphertext to exactly one wrapped key, so a row cannot be paired with another row's wrapped DEK, and
// binding role, purpose, version and KEK version means a row moved between roles, versions or KEKs does not authenticate.
func AAD(role, purpose string, version, kekVersion int, wrappedDEK []byte) []byte {
	h := sha256.Sum256(wrappedDEK)
	return []byte(fmt.Sprintf("%s|%s|%d|%d|%s", role, purpose, version, kekVersion, hex.EncodeToString(h[:])))
}

// Row is a stored Sealed Secret as the callers send it back (base64 in JSON).
type Row struct {
	Role       string `json:"role"`
	Purpose    string `json:"purpose"`
	Version    int    `json:"version"`
	KEKVersion int    `json:"kek_version"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	AAD        []byte `json:"aad"`
}

// SealRequest is the body of POST /v1/vault/seal. KEKVersion 0 means the newest wrapping KEK.
type SealRequest struct {
	Role       string `json:"role"`
	Purpose    string `json:"purpose"`
	Version    int    `json:"version"`
	KEKVersion int    `json:"kek_version,omitempty"`
	Plaintext  []byte `json:"plaintext"`
}

// ResealRequest is the body of POST /v1/vault/reseal: Row is the stored row, ToVersion the row version it becomes, ToKEK the KEK that wraps
// it (0 = newest wrapping), NewDEK whether the DEK is replaced too (DEK rotation) or only re-wrapped (KEK rotation).
type ResealRequest struct {
	Row       Row  `json:"row"`
	ToVersion int  `json:"to_version"`
	ToKEK     int  `json:"to_kek_version,omitempty"`
	NewDEK    bool `json:"new_dek"`
}

// KEKRequest is the body of the KEK admin operations.
type KEKRequest struct {
	Version int `json:"version"`
}

func decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrShape, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data after the JSON object", ErrShape)
	}
	return nil
}

// DecodeRow reads a Row (unseal and verify bodies).
func DecodeRow(r io.Reader) (*Row, error) {
	var x Row
	if err := decode(r, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// DecodeSeal reads a SealRequest.
func DecodeSeal(r io.Reader) (*SealRequest, error) {
	var x SealRequest
	if err := decode(r, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// DecodeReseal reads a ResealRequest.
func DecodeReseal(r io.Reader) (*ResealRequest, error) {
	var x ResealRequest
	if err := decode(r, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// DecodeKEK reads a KEKRequest.
func DecodeKEK(r io.Reader) (*KEKRequest, error) {
	var x KEKRequest
	if err := decode(r, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// Fingerprint is a short non-secret identifier of the wrapped DEK for the audit line.
func Fingerprint(wrappedDEK []byte) string {
	h := sha256.Sum256(wrappedDEK)
	return hex.EncodeToString(h[:6])
}

// SameAAD compares in constant shape (the AAD is not secret, this is only equality).
func SameAAD(a, b []byte) bool { return bytes.Equal(a, b) }
