// Package xpub is the seed-free half of deposit-address issuance (key-custody-hsm.md M3a, invariants 9-13): the xpub
// Attestation statement and its verification against a public key pinned outside the database, and public (BIP32 CKDpub)
// child derivation from the attested account xpub. Nothing here can hold or derive a private key: it imports no signing
// code, only btcd's hdkeychain used through public derivation, and it is compiled into the production binary.
//
// Why EVM only (invariant 13): public child derivation exists for secp256k1 chains (EVM coin 60', Bitcoin 0', Tron 195').
// It does not exist for ed25519: SLIP-10 ed25519 derivation is hardened-only, so a Solana address cannot be derived from a
// public key at all. Solana would need a pre-generated pool of public addresses or a generator that holds a secret, which
// contradicts the premise of this package; it is a new design, and wallet_xpubs.chain has CHECK (chain = 'evm').
package xpub

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

// The statement. It is built the same way by hsm-gateway (services/hsm-gateway/internal/attest); both modules pin one
// golden vector so a drift in either breaks a test.
const (
	StatementVersion = "digiwal/xpub-attest/v1"
	ChainEVM         = "evm"
	// PathEVM is the account node: m/44'/60'/0', depth 3, hardened child number 0.
	PathEVM = "m/44'/60'/0'"
	// AccountDepth is the BIP32 depth of the account node.
	AccountDepth = 3
	// Alg: ECDSA over NIST P-256 of SHA-256(statement); the signature is the raw 64-byte r||s.
	Alg = "ecdsa-p256-sha256"
)

// Record is one wallet_xpubs row as far as verification goes.
type Record struct {
	Chain       string
	Path        string
	XPub        string
	Attestation []byte
}

// Statement is the canonical byte string the HSM signs: fixed order, one field per line, trailing newline.
func Statement(chain, path, xpub string) []byte {
	var b bytes.Buffer
	b.WriteString(StatementVersion + "\n")
	b.WriteString("chain=" + chain + "\n")
	b.WriteString("path=" + path + "\n")
	b.WriteString("xpub=" + xpub + "\n")
	return b.Bytes()
}

// Digest is SHA-256(Statement(...)).
func Digest(chain, path, xpub string) [32]byte { return sha256.Sum256(Statement(chain, path, xpub)) }

// Fingerprint is a short non-secret identifier for logs (the xpub itself is never logged).
func Fingerprint(xpub string) string {
	s := sha256.Sum256([]byte(xpub))
	return hex.EncodeToString(s[:6])
}

// ErrAttestation is wrapped by every verification failure (a bad signature, a malformed record, a wrong shape).
var ErrAttestation = errors.New("xpub attestation invalid")

// ParseAccountKey parses r.XPub and requires exactly the account node: a canonical mainnet PUBLIC extended key, depth 3,
// hardened child number 0.
func ParseAccountKey(xpub string) (*hdkeychain.ExtendedKey, error) {
	k, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, fmt.Errorf("%w: xpub does not parse: %v", ErrAttestation, err)
	}
	switch {
	case k.IsPrivate():
		return nil, fmt.Errorf("%w: the stored key is a private extended key", ErrAttestation)
	case !k.IsForNet(&chaincfg.MainNetParams):
		return nil, fmt.Errorf("%w: not an xpub (mainnet public version bytes)", ErrAttestation)
	case k.Depth() != AccountDepth:
		return nil, fmt.Errorf("%w: depth %d, want %d", ErrAttestation, k.Depth(), AccountDepth)
	case k.ChildIndex() != hdkeychain.HardenedKeyStart:
		return nil, fmt.Errorf("%w: child number %d, want hardened 0", ErrAttestation, k.ChildIndex())
	case k.String() != xpub:
		return nil, fmt.Errorf("%w: xpub is not in canonical encoding", ErrAttestation)
	}
	return k, nil
}

// Verify checks a record against the pinned attestation public key: chain and path are exactly the allowed ones, the xpub is
// the account node, and the signature is a valid ECDSA P-256 signature over SHA-256 of the canonical statement. It returns
// the parsed account key so the caller derives from exactly the bytes that were verified.
func Verify(pin *ecdsa.PublicKey, r Record) (*hdkeychain.ExtendedKey, error) {
	if pin == nil {
		return nil, fmt.Errorf("%w: no pinned attestation public key", ErrAttestation)
	}
	if r.Chain != ChainEVM || r.Path != PathEVM {
		return nil, fmt.Errorf("%w: chain %q path %q", ErrAttestation, r.Chain, r.Path)
	}
	if len(r.Attestation) != 64 {
		return nil, fmt.Errorf("%w: attestation is %d bytes, want 64", ErrAttestation, len(r.Attestation))
	}
	k, err := ParseAccountKey(r.XPub)
	if err != nil {
		return nil, err
	}
	d := Digest(r.Chain, r.Path, r.XPub)
	if !ecdsa.Verify(pin, d[:], new(big.Int).SetBytes(r.Attestation[:32]), new(big.Int).SetBytes(r.Attestation[32:])) {
		return nil, fmt.Errorf("%w: signature does not verify under the pinned public key", ErrAttestation)
	}
	return k, nil
}

// ParsePinned reads the pinned attestation public key. Accepted encodings: the hex of the 65-byte uncompressed P-256 point
// (what `hsm-gateway init-token` prints as attest_public_key=), or a PKIX "PUBLIC KEY" PEM. Anything else, and any key that is
// not a point on P-256, is an error: a pin that cannot be read must stop the service, never disable verification.
func ParsePinned(b []byte) (*ecdsa.PublicKey, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "-----BEGIN") {
		blk, _ := pem.Decode([]byte(s))
		if blk == nil || blk.Type != "PUBLIC KEY" {
			return nil, errors.New("pinned attestation key: not a PUBLIC KEY PEM block")
		}
		pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pinned attestation key: %w", err)
		}
		ec, ok := pk.(*ecdsa.PublicKey)
		if !ok || ec.Curve != elliptic.P256() {
			return nil, errors.New("pinned attestation key: not an ECDSA P-256 key")
		}
		return ec, nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "attest_public_key="), "0x")
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		return nil, errors.New("pinned attestation key: want the hex of a 65-byte uncompressed P-256 point or a PUBLIC KEY PEM")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), raw) //nolint:staticcheck // the on-curve check is the point
	if x == nil {
		return nil, errors.New("pinned attestation key: the point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

// LoadPinned reads and parses the pin file.
func LoadPinned(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the pinned attestation key: %w", err)
	}
	return ParsePinned(b)
}

// PinnedHex is the canonical hex form of a pinned key (65-byte uncompressed point), for comparisons and logs.
func PinnedHex(pk *ecdsa.PublicKey) string {
	return hex.EncodeToString(elliptic.Marshal(elliptic.P256(), pk.X, pk.Y)) //nolint:staticcheck
}
