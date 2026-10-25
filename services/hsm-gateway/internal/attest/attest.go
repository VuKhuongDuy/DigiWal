// Package attest is the xpub Attestation statement (key-custody-hsm.md invariant 10): the canonical
// bytes the attestation key signs, and the strict validation of what a caller may ask to have signed.
//
// The gateway never signs bytes a caller supplies. A caller names (chain family, path, xpub); the
// gateway checks that the triple has exactly the shape the design allows, builds the canonical
// statement itself and signs SHA-256(statement). Anything else (another chain, another path, a
// private key, a child at the wrong depth, extra JSON fields) is refused and audited.
//
// The wallet-service verifier carries the same statement builder (services/wallet-service/internal/xpub);
// both modules pin the same golden vector so a drift in either breaks a test.
package attest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

// Statement constants. Version is a domain separator: a signature over a different protocol's bytes can
// never be replayed as an attestation because every statement starts with it.
const (
	Version  = "digiwal/xpub-attest/v1"
	ChainEVM = "evm"
	// PathEVM is the account node: m/44'/60'/0' (EVM coin type 60, account 0), depth 3, hardened child 0.
	PathEVM = "m/44'/60'/0'"
	// Alg names the signature: ECDSA over NIST P-256 of SHA-256(statement), raw 64-byte r||s.
	Alg = "ecdsa-p256-sha256"
)

// ErrShape is returned for any request that is not exactly the allowed shape.
var ErrShape = errors.New("attest: refused statement shape")

// Request is the body of POST /v1/attest. It has no digest, hash or signature field on purpose.
type Request struct {
	Chain string `json:"chain"`
	Path  string `json:"path"`
	XPub  string `json:"xpub"`
}

// Decode reads exactly one JSON object with no unknown fields and nothing after it.
func Decode(r io.Reader) (*Request, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var q Request
	if err := dec.Decode(&q); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShape, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data after the JSON object", ErrShape)
	}
	return &q, nil
}

// Validate refuses everything but: chain evm, path m/44'/60'/0', and an xpub that is a canonical mainnet
// public extended key at depth 3 whose child number is hardened 0 (the account node).
func (q *Request) Validate() error {
	if q.Chain != ChainEVM {
		return fmt.Errorf("%w: chain %q (only %q: secp256k1 public derivation exists for EVM, not for ed25519 chains)", ErrShape, q.Chain, ChainEVM)
	}
	if q.Path != PathEVM {
		return fmt.Errorf("%w: path %q (only %q)", ErrShape, q.Path, PathEVM)
	}
	k, err := hdkeychain.NewKeyFromString(q.XPub)
	if err != nil {
		return fmt.Errorf("%w: xpub does not parse: %v", ErrShape, err)
	}
	switch {
	case k.IsPrivate():
		return fmt.Errorf("%w: the key is private, only an xpub may be attested", ErrShape)
	case !k.IsForNet(&chaincfg.MainNetParams):
		return fmt.Errorf("%w: not an xpub (mainnet public version bytes)", ErrShape)
	case k.Depth() != 3:
		return fmt.Errorf("%w: depth %d, want 3 (the account node)", ErrShape, k.Depth())
	case k.ChildIndex() != hdkeychain.HardenedKeyStart:
		return fmt.Errorf("%w: child number %d, want hardened 0", ErrShape, k.ChildIndex())
	case k.String() != q.XPub:
		return fmt.Errorf("%w: xpub is not in canonical encoding", ErrShape)
	}
	return nil
}

// Statement is the canonical byte string that is signed. Line-oriented, fixed order, trailing newline.
func (q *Request) Statement() []byte {
	var b bytes.Buffer
	b.WriteString(Version + "\n")
	b.WriteString("chain=" + q.Chain + "\n")
	b.WriteString("path=" + q.Path + "\n")
	b.WriteString("xpub=" + q.XPub + "\n")
	return b.Bytes()
}

// Digest is SHA-256(Statement()), the 32 bytes handed to the token.
func (q *Request) Digest() [32]byte { return sha256.Sum256(q.Statement()) }

// Fingerprint is a short, non-secret identifier of the xpub for logs and the audit line (the xpub itself is never logged).
func (q *Request) Fingerprint() string {
	s := sha256.Sum256([]byte(q.XPub))
	return fmt.Sprintf("%x", s[:6])
}
