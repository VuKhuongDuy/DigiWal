//go:build !prod

package keyadmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"

	"digiwal/services/wallet-service/internal/xpub"
)

// DevAttester stands in for hsm-gateway's attestation key in DEV and e2e environments that have NO HSM (SIGNER_BACKEND=software):
// an in-process P-256 key that signs the same statement and is thrown away when the process exits. Since M3b wallet-service has
// no seed and can issue addresses only from an attested xpub, such an environment needs an attestation too; this gives it one
// without a token. It is a `!prod` ceremony helper: its key is ephemeral, its public half is what gets pinned, and nothing about
// it is a security claim (the pinned key lives next to the database it protects, so it only exercises the verification path).
type DevAttester struct{ key *ecdsa.PrivateKey }

// NewDevAttester generates a fresh key.
func NewDevAttester() (*DevAttester, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &DevAttester{key: k}, nil
}

// PublicHex is the 65-byte uncompressed point as hex, the form infra/hsm/pin-attest-key.sh pins.
func (d *DevAttester) PublicHex() string { return xpub.PinnedHex(&d.key.PublicKey) }

// Public is the public key (to pass as the pin of the migration).
func (d *DevAttester) Public() *ecdsa.PublicKey { return &d.key.PublicKey }

func (d *DevAttester) pub() []byte {
	return elliptic.Marshal(elliptic.P256(), d.key.X, d.key.Y) //nolint:staticcheck
}

// AttestKey implements Gateway.
func (d *DevAttester) AttestKey(context.Context) ([]byte, error) { return d.pub(), nil }

// Attest implements Gateway: raw r||s over SHA-256 of the canonical statement.
func (d *DevAttester) Attest(_ context.Context, chain, path, xp string) ([]byte, []byte, error) {
	dg := xpub.Digest(chain, path, xp)
	r, s, err := ecdsa.Sign(rand.Reader, d.key, dg[:])
	if err != nil {
		return nil, nil, fmt.Errorf("dev attestation: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, d.pub(), nil
}
