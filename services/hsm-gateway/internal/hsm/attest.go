package hsm

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"fmt"
	"math/big"

	"github.com/miekg/pkcs11"
)

// Attestor is the optional second capability of the token: signing xpub Attestations with a dedicated
// P-256 key that is not the Hot Key (key-custody-hsm.md invariant 10). *Token implements it; the Backend
// interface (Hot signing) is untouched so the Hot path cannot reach this key.
type Attestor interface {
	// HasAttestKey is false on a token that was initialised before M3a and has no attestation key yet.
	HasAttestKey() bool
	// AttestPublicKey is the 65-byte uncompressed P-256 public key (nil when there is no key).
	AttestPublicKey() []byte
	// SignAttestation signs a 32-byte SHA-256 digest and returns the raw 64-byte r||s.
	SignAttestation(ctx context.Context, digest []byte) ([]byte, error)
	// CheckAttestKey re-reads the key attributes and fails unless they are the required ones.
	CheckAttestKey(ctx context.Context) error
}

var _ Attestor = (*Token)(nil)

// locateAttest finds the attestation key pair by label. Absent is fine (attestation unavailable); present
// with the wrong attributes, curve or in duplicate is refused.
func (t *Token) locateAttest(ctx context.Context, label string) error {
	priv, n, err := t.findCount(ctx, pkcs11.CKO_PRIVATE_KEY, label)
	if err != nil {
		return err
	}
	switch {
	case n == 0:
		return nil
	case n > 1:
		return &Error{Code: CodeKeyAttrs, Op: "find attestation key", Cause: fmt.Errorf("%d private keys labelled %q", n, label)}
	}
	pub, err := t.find(ctx, pkcs11.CKO_PUBLIC_KEY, label)
	if err != nil {
		return err
	}
	var attrs []*pkcs11.Attribute
	if err := t.withSession(ctx, "read attestation public key", func(s pkcs11.SessionHandle) error {
		var e error
		attrs, e = t.ctx.GetAttributeValue(s, pub, []*pkcs11.Attribute{
			pkcs11.NewAttribute(pkcs11.CKA_EC_POINT, nil), pkcs11.NewAttribute(pkcs11.CKA_EC_PARAMS, nil)})
		return e
	}); err != nil {
		return err
	}
	var point, params []byte
	for _, a := range attrs {
		switch a.Type {
		case pkcs11.CKA_EC_POINT:
			point = a.Value
		case pkcs11.CKA_EC_PARAMS:
			params = a.Value
		}
	}
	bad := func(e error) error { return &Error{Code: CodeKeyAttrs, Op: "read attestation public key", Cause: e} }
	if !bytes.Equal(params, p256OID) {
		return bad(errors.New("the attestation key is not on P-256"))
	}
	raw, err := ParseECPoint(point)
	if err != nil {
		return bad(err)
	}
	if x, y := elliptic.Unmarshal(elliptic.P256(), raw); x == nil || y == nil { //nolint:staticcheck // on-curve check only
		return bad(errors.New("the attestation public point is not on P-256"))
	}
	if bytes.Equal(raw, t.pubKey) {
		return bad(errors.New("the attestation key is the Hot Key"))
	}
	t.attPriv, t.attPub, t.hasAttest = priv, raw, true
	return t.CheckAttestKey(ctx)
}

// HasAttestKey implements Attestor.
func (t *Token) HasAttestKey() bool { return t.hasAttest }

// AttestPublicKey implements Attestor.
func (t *Token) AttestPublicKey() []byte { return append([]byte(nil), t.attPub...) }

// CheckAttestKey implements Attestor: the same attribute rules as the Hot Key (CKA_PRIVATE=true, sensitive,
// never extractable, sign, EC).
func (t *Token) CheckAttestKey(ctx context.Context) error {
	if !t.hasAttest {
		return &Error{Code: CodeKeyMissing, Op: "check attestation key", Cause: errors.New("the token has no attestation key")}
	}
	var attrs []*pkcs11.Attribute
	err := t.withSession(ctx, "read attestation key attributes", func(s pkcs11.SessionHandle) error {
		var e error
		attrs, e = t.ctx.GetAttributeValue(s, t.attPriv, []*pkcs11.Attribute{
			pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, nil), pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, nil),
			pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, nil), pkcs11.NewAttribute(pkcs11.CKA_SIGN, nil),
			pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, nil), pkcs11.NewAttribute(pkcs11.CKA_NEVER_EXTRACTABLE, nil)})
		return e
	})
	if err != nil {
		return err
	}
	return checkAttrs(attrs)
}

// SignAttestation implements Attestor. The signature is verified against the token's own public key before
// it is returned: a wrong key or a corrupted output is never handed out.
func (t *Token) SignAttestation(ctx context.Context, digest []byte) ([]byte, error) {
	if !t.hasAttest {
		return nil, &Error{Code: CodeKeyMissing, Op: "attest", Cause: errors.New("the token has no attestation key")}
	}
	if len(digest) != 32 {
		return nil, &Error{Code: CodeBadOutput, Op: "attest", Cause: fmt.Errorf("digest is %d bytes", len(digest))}
	}
	var rs []byte
	err := t.withSession(ctx, "attest", func(s pkcs11.SessionHandle) error {
		if err := t.ctx.SignInit(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_ECDSA, nil)}, t.attPriv); err != nil {
			return err
		}
		var err error
		rs, err = t.ctx.Sign(s, digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(rs) != 64 {
		return nil, &Error{Code: CodeBadOutput, Op: "attest", Cause: fmt.Errorf("token returned %d bytes, want 64", len(rs))}
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), t.attPub) //nolint:staticcheck
	pk := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	if !ecdsa.Verify(pk, digest, new(big.Int).SetBytes(rs[:32]), new(big.Int).SetBytes(rs[32:])) {
		return nil, &Error{Code: CodeBadOutput, Op: "attest", Cause: errors.New("the token's signature does not verify under its own attestation public key")}
	}
	return rs, nil
}
