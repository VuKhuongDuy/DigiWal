package hsm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/miekg/pkcs11"
)

// The vault half of the token (key-custody-hsm.md invariants 18-20, M3c): a KEK (AES-256, generated inside the token) wraps a
// per-secret DEK; the DEK is only ever a session object inside the token (SENSITIVE, never readable); AES-256-GCM runs INSIDE the
// token (CKM_AES_GCM). Spike M0 (a): SoftHSM accepts a zero-length GCM IV and never generates one, so this code owns the nonce
// (12 random bytes from crypto/rand) and refuses every other nonce on every path (CheckNonce). Spike M0 (d): CKA_PRIVATE=true is
// explicit on every key, because a non-private key is stored in plaintext in the token files.

// KEKLabelPrefix is the label of a KEK object: kek-v1, kek-v2, ...
const KEKLabelPrefix = "kek-v"

// MaxKEKVersion bounds the label probe (PKCS#11 has no prefix search). A rotation yearly would take 64 years to reach it.
const MaxKEKVersion = 64

// NonceSize is the GCM nonce length this gateway accepts: 96 bits, no other.
const NonceSize = 12

// tagBits is the GCM tag length.
const tagBits = 128

// ErrAuthFailed is the explicit "authentication failed" of every tamper: wrong AAD, flipped ciphertext bit, a wrapped DEK that does
// not unwrap, a KEK that is not the one that wrapped it. SoftHSM reports all of these as CKR_GENERAL_ERROR (M0 (a)); the token layer
// converts them here so a caller never sees an untyped PKCS#11 failure for a tamper.
var ErrAuthFailed = errors.New("authentication failed")

// ErrNoKEK: the token has no (usable) KEK, or not the requested version.
var ErrNoKEK = errors.New("no such KEK")

// ErrBadNonce: a GCM nonce that is not 96 bits of non-zero data.
var ErrBadNonce = errors.New("GCM nonce must be exactly 12 bytes and not all zero")

// KEKInfo describes one KEK object.
type KEKInfo struct {
	Version int  `json:"version"`
	Wrap    bool `json:"wrap"` // CKA_WRAP: false once retired (unwrap-only)
}

// Sealed is the output of Seal and Reseal. WrappedDEK, Nonce, Ciphertext (including the 16-byte tag) and AAD are what the caller stores.
type Sealed struct {
	KEKVersion int
	WrappedDEK []byte
	Nonce      []byte
	Ciphertext []byte
	AAD        []byte
}

// Opened is what Unseal/Reseal need of a stored row.
type Opened struct {
	KEKVersion int
	WrappedDEK []byte
	Nonce      []byte
	Ciphertext []byte
	AAD        []byte
}

// AADFunc builds the AAD from the wrapped DEK (the AAD binds SHA256(wrapped_dek)), so it can only be known after wrapping.
type AADFunc func(wrappedDEK []byte) []byte

// Vault is the KEK/DEK capability of a token. *Token implements it; the Hot signing Backend is untouched.
type Vault interface {
	// HasKEK is false on a token created before M3c until `init-kek` has run.
	HasKEK() bool
	// KEKs lists the KEK versions in the token, ascending, each attribute-checked (CKA_PRIVATE, sensitive, non-extractable...).
	KEKs(ctx context.Context) ([]KEKInfo, error)
	// GenerateKEK creates kek-v{version} inside the token. version must be 1 or the highest existing + 1. An existing, correct
	// object of that version is not an error (created=false): the call is idempotent.
	GenerateKEK(ctx context.Context, version int) (created bool, err error)
	// RetireKEK makes kek-v{version} unwrap-only (CKA_WRAP=false). Idempotent. Refused for the highest version (it would leave nothing to seal with).
	RetireKEK(ctx context.Context, version int) error
	// DestroyKEK deletes kek-v{version}. Refused unless it is retired and not the highest.
	DestroyKEK(ctx context.Context, version int) error
	// Seal generates a DEK in the token, wraps it with kek-v{kekVersion}, GCM-encrypts plaintext with it and destroys the DEK object.
	Seal(ctx context.Context, kekVersion int, plaintext []byte, aad AADFunc) (*Sealed, error)
	// Unseal unwraps the DEK with the KEK recorded in o, GCM-decrypts inside the token and returns the plaintext. aadOf is the AAD the
	// caller expects; it must equal o.AAD or the call fails with ErrAuthFailed before the token is asked.
	Unseal(ctx context.Context, o Opened, expectAAD []byte) ([]byte, error)
	// Reseal decrypts o inside the token and encrypts it again, under a new nonce and the AAD aad builds, wrapped by kek-v{toKEK}.
	// newDEK=true also replaces the DEK (DEK rotation); false keeps the DEK and only re-wraps it (KEK rotation).
	Reseal(ctx context.Context, o Opened, expectAAD []byte, toKEK int, newDEK bool, aad AADFunc) (*Sealed, error)
}

var _ Vault = (*Token)(nil)

// CheckNonce is the one place a GCM nonce is validated. Called on every encrypt path and on every row that is unsealed.
func CheckNonce(n []byte) error {
	if len(n) != NonceSize || bytes.Equal(n, make([]byte, NonceSize)) {
		return ErrBadNonce
	}
	return nil
}

// newNonce is 12 random bytes. A var so a test can inject a broken source and prove the refusal.
var newNonce = func() ([]byte, error) {
	n := make([]byte, NonceSize)
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return n, nil
}

func kekLabel(v int) string { return fmt.Sprintf("%s%d", KEKLabelPrefix, v) }

// HasKEK implements Vault.
func (t *Token) HasKEK() bool {
	ks, err := t.KEKs(context.Background())
	return err == nil && len(ks) > 0
}

// findKEK returns the handle of kek-v{v} (exactly one object) on session s. ErrNoKEK if absent.
func (t *Token) findKEK(s pkcs11.SessionHandle, v int) (pkcs11.ObjectHandle, error) {
	if v < 1 || v > MaxKEKVersion {
		return 0, fmt.Errorf("%w: version %d", ErrNoKEK, v)
	}
	tmpl := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, kekLabel(v)),
	}
	if err := t.ctx.FindObjectsInit(s, tmpl); err != nil {
		return 0, err
	}
	objs, _, err := t.ctx.FindObjects(s, 3)
	_ = t.ctx.FindObjectsFinal(s)
	if err != nil {
		return 0, err
	}
	switch len(objs) {
	case 0:
		return 0, fmt.Errorf("%w: %s", ErrNoKEK, kekLabel(v))
	case 1:
		return objs[0], nil
	}
	return 0, &Error{Code: CodeKeyAttrs, Op: "find KEK", Cause: fmt.Errorf("%d objects labelled %q", len(objs), kekLabel(v))}
}

func (t *Token) kekAttrs(s pkcs11.SessionHandle, h pkcs11.ObjectHandle) ([]*pkcs11.Attribute, error) {
	return t.ctx.GetAttributeValue(s, h, []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, nil), pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, nil), pkcs11.NewAttribute(pkcs11.CKA_NEVER_EXTRACTABLE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, nil), pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE_LEN, nil), pkcs11.NewAttribute(pkcs11.CKA_WRAP, nil),
		pkcs11.NewAttribute(pkcs11.CKA_UNWRAP, nil), pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, nil),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, nil), pkcs11.NewAttribute(pkcs11.CKA_SIGN, nil)})
}

// checkKEKAttrs refuses a KEK that is not: a persistent, private (M0 (d)), sensitive, never-extractable AES-256 key that can unwrap
// and cannot encrypt, decrypt or sign directly. It returns the CKA_WRAP flag.
func checkKEKAttrs(attrs []*pkcs11.Attribute) (wrap bool, err error) {
	got := map[uint][]byte{}
	for _, a := range attrs {
		got[a.Type] = a.Value
	}
	isTrue := func(t uint) bool { v := got[t]; return len(v) > 0 && v[0] != 0 }
	bad := func(why string) error {
		return &Error{Code: CodeKeyAttrs, Op: "check KEK attributes", Cause: errors.New(why)}
	}
	switch {
	case !isTrue(pkcs11.CKA_TOKEN):
		return false, bad("CKA_TOKEN is not true (the KEK would not survive a restart)")
	case !isTrue(pkcs11.CKA_PRIVATE):
		return false, bad("CKA_PRIVATE is not true (the KEK would be stored in plaintext in the token files)")
	case !isTrue(pkcs11.CKA_SENSITIVE):
		return false, bad("CKA_SENSITIVE is not true")
	case isTrue(pkcs11.CKA_EXTRACTABLE):
		return false, bad("CKA_EXTRACTABLE is true")
	case !isTrue(pkcs11.CKA_NEVER_EXTRACTABLE):
		return false, bad("CKA_NEVER_EXTRACTABLE is not true (the KEK was extractable at some point)")
	case !isTrue(pkcs11.CKA_UNWRAP):
		return false, bad("CKA_UNWRAP is not true")
	case isTrue(pkcs11.CKA_ENCRYPT), isTrue(pkcs11.CKA_DECRYPT), isTrue(pkcs11.CKA_SIGN):
		return false, bad("the KEK can encrypt, decrypt or sign directly: it must only wrap and unwrap")
	case ulong(got[pkcs11.CKA_KEY_TYPE]) != pkcs11.CKK_AES:
		return false, bad("key type is not AES")
	case ulong(got[pkcs11.CKA_VALUE_LEN]) != 32:
		return false, bad("the KEK is not 256 bits")
	}
	return isTrue(pkcs11.CKA_WRAP), nil
}

// KEKs implements Vault.
func (t *Token) KEKs(ctx context.Context) ([]KEKInfo, error) {
	var out []KEKInfo
	err := t.withSession(ctx, "list KEKs", func(s pkcs11.SessionHandle) error {
		out = out[:0]
		for v := 1; v <= MaxKEKVersion; v++ {
			h, err := t.findKEK(s, v)
			if errors.Is(err, ErrNoKEK) {
				continue
			}
			if err != nil {
				return err
			}
			attrs, err := t.kekAttrs(s, h)
			if err != nil {
				return err
			}
			wrap, err := checkKEKAttrs(attrs)
			if err != nil {
				return fmt.Errorf("%s: %w", kekLabel(v), err)
			}
			out = append(out, KEKInfo{Version: v, Wrap: wrap})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func kekTemplate(v int) []*pkcs11.Attribute {
	return []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE_LEN, 32),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true), // explicit: M0 (d), private=false is plaintext at rest
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, false),
		pkcs11.NewAttribute(pkcs11.CKA_WRAP, true),
		pkcs11.NewAttribute(pkcs11.CKA_UNWRAP, true),
		pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, false),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, false),
		pkcs11.NewAttribute(pkcs11.CKA_SIGN, false), // SoftHSM defaults SIGN/VERIFY/DERIVE to true: say so explicitly
		pkcs11.NewAttribute(pkcs11.CKA_VERIFY, false),
		pkcs11.NewAttribute(pkcs11.CKA_DERIVE, false),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, kekLabel(v)),
	}
}

// rwSession runs fn on a fresh read-write session of the token-wide login. Pool sessions are read-only; token objects are
// created, modified and destroyed on a dedicated short-lived RW session (login state is token-wide).
func (t *Token) rwSession(ctx context.Context, op string, fn func(s pkcs11.SessionHandle) error) error {
	if t.closed.Load() {
		return &Error{Code: CodeUnavailable, Op: op, Cause: errors.New("token closed")}
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := t.pool.Quiesce(qctx)
	if err != nil {
		return &Error{Code: CodeUnavailable, Op: op, Cause: fmt.Errorf("waiting for the HSM sessions to go idle: %w", err)}
	}
	defer release()
	s, err := t.ctx.OpenSession(t.slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		return Map("OpenSession", err)
	}
	defer func() { _ = t.ctx.CloseSession(s) }()
	if err := fn(s); err != nil {
		return Map(op, err)
	}
	return nil
}

// GenerateKEK implements Vault.
func (t *Token) GenerateKEK(ctx context.Context, version int) (bool, error) {
	if version < 1 || version > MaxKEKVersion {
		return false, fmt.Errorf("KEK version %d out of range 1..%d", version, MaxKEKVersion)
	}
	ks, err := t.KEKs(ctx)
	if err != nil {
		return false, err
	}
	hi := 0
	for _, k := range ks {
		if k.Version == version {
			return false, nil // exists and passed checkKEKAttrs in KEKs(): idempotent
		}
		if k.Version > hi {
			hi = k.Version
		}
	}
	if version != hi+1 {
		return false, fmt.Errorf("KEK version %d refused: the next version is %d (versions are consecutive)", version, hi+1)
	}
	err = t.rwSession(ctx, "generate KEK", func(s pkcs11.SessionHandle) error {
		h, err := t.ctx.GenerateKey(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil)}, kekTemplate(version))
		if err != nil {
			return fmt.Errorf("C_GenerateKey: %w", err)
		}
		attrs, err := t.kekAttrs(s, h)
		if err != nil {
			return fmt.Errorf("reading the new KEK back: %w", err)
		}
		if _, err := checkKEKAttrs(attrs); err != nil {
			_ = t.ctx.DestroyObject(s, h) // never leave a KEK that fails its own checks
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	log.Printf("vault: generated %s inside the token (AES-256, private, sensitive, non-extractable)", kekLabel(version))
	return true, nil
}

// RetireKEK implements Vault.
func (t *Token) RetireKEK(ctx context.Context, version int) error {
	ks, err := t.KEKs(ctx)
	if err != nil {
		return err
	}
	hi, found, wrap := 0, false, false
	for _, k := range ks {
		if k.Version > hi {
			hi = k.Version
		}
		if k.Version == version {
			found, wrap = true, k.Wrap
		}
	}
	switch {
	case !found:
		return fmt.Errorf("%w: %s", ErrNoKEK, kekLabel(version))
	case !wrap:
		return nil
	case version == hi:
		return fmt.Errorf("KEK %s is the newest: retiring it would leave no key to seal with", kekLabel(version))
	}
	return t.rwSession(ctx, "retire KEK", func(s pkcs11.SessionHandle) error {
		h, err := t.findKEK(s, version)
		if err != nil {
			return err
		}
		if err := t.ctx.SetAttributeValue(s, h, []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_WRAP, false)}); err != nil {
			return err
		}
		attrs, err := t.kekAttrs(s, h)
		if err != nil {
			return err
		}
		if w, err := checkKEKAttrs(attrs); err != nil || w {
			return fmt.Errorf("the token did not apply CKA_WRAP=false (wrap=%v): %v", w, err)
		}
		return nil
	})
}

// DestroyKEK implements Vault.
func (t *Token) DestroyKEK(ctx context.Context, version int) error {
	ks, err := t.KEKs(ctx)
	if err != nil {
		return err
	}
	hi, found, wrap := 0, false, false
	for _, k := range ks {
		if k.Version > hi {
			hi = k.Version
		}
		if k.Version == version {
			found, wrap = true, k.Wrap
		}
	}
	switch {
	case !found:
		return fmt.Errorf("%w: %s", ErrNoKEK, kekLabel(version))
	case wrap:
		return fmt.Errorf("KEK %s is not retired (still wraps): retire it first", kekLabel(version))
	case version == hi:
		return fmt.Errorf("KEK %s is the newest and cannot be destroyed", kekLabel(version))
	}
	return t.rwSession(ctx, "destroy KEK", func(s pkcs11.SessionHandle) error {
		h, err := t.findKEK(s, version)
		if err != nil {
			return err
		}
		return t.ctx.DestroyObject(s, h)
	})
}

func dekTemplate() []*pkcs11.Attribute {
	return []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE_LEN, 32),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false), // a session object: gone when the session ends
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),   // the value can never be read
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, true), // only so it can be WRAPPED; SENSITIVE keeps it unreadable (M0 (a))
		pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, true),
	}
}

// unwrapTemplate has NO CKA_VALUE_LEN (M0 (a): CKR_ATTRIBUTE_READ_ONLY).
func unwrapTemplate(extractable bool) []*pkcs11.Attribute {
	return []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false),
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, extractable),
		pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, true),
	}
}

var wrapMech = func() []*pkcs11.Mechanism {
	return []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_WRAP, nil)}
}

// gcm runs one CKM_AES_GCM operation with the caller-chosen nonce and AAD. The nonce is validated first: this function can not be
// reached with a zero or short IV.
func (t *Token) gcmEncrypt(s pkcs11.SessionHandle, dek pkcs11.ObjectHandle, nonce, aad, pt []byte) ([]byte, error) {
	if err := CheckNonce(nonce); err != nil {
		return nil, err
	}
	p := pkcs11.NewGCMParams(nonce, aad, tagBits)
	defer p.Free()
	if err := t.ctx.EncryptInit(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, p)}, dek); err != nil {
		return nil, err
	}
	return t.ctx.Encrypt(s, pt)
}

func (t *Token) gcmDecrypt(s pkcs11.SessionHandle, dek pkcs11.ObjectHandle, nonce, aad, ct []byte) ([]byte, error) {
	if err := CheckNonce(nonce); err != nil {
		return nil, err
	}
	p := pkcs11.NewGCMParams(nonce, aad, tagBits)
	defer p.Free()
	if err := t.ctx.DecryptInit(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, p)}, dek); err != nil {
		return nil, err
	}
	pt, err := t.ctx.Decrypt(s, ct)
	if err != nil {
		// M0 (a): a wrong AAD, a wrong key or a flipped bit is CKR_GENERAL_ERROR on SoftHSM (not a typed error). The DEK unwrapped, so
		// the token works: the only thing that differs is the data. Say so explicitly.
		var pe pkcs11.Error
		if errors.As(err, &pe) {
			switch uint(pe) {
			case pkcs11.CKR_GENERAL_ERROR, pkcs11.CKR_ENCRYPTED_DATA_INVALID, pkcs11.CKR_ENCRYPTED_DATA_LEN_RANGE, pkcs11.CKR_DATA_INVALID, pkcs11.CKR_SIGNATURE_INVALID:
				return nil, ErrAuthFailed
			}
		}
		return nil, err
	}
	return pt, nil
}

// kekForWrap finds kek-v{v} and requires CKA_WRAP.
func (t *Token) kekForWrap(s pkcs11.SessionHandle, v int) (pkcs11.ObjectHandle, error) {
	h, err := t.findKEK(s, v)
	if err != nil {
		return 0, err
	}
	attrs, err := t.kekAttrs(s, h)
	if err != nil {
		return 0, err
	}
	wrap, err := checkKEKAttrs(attrs)
	if err != nil {
		return 0, err
	}
	if !wrap {
		return 0, fmt.Errorf("KEK %s is retired (unwrap-only): it cannot seal or re-wrap", kekLabel(v))
	}
	return h, nil
}

// kekForUnwrap finds kek-v{v} (wrapping or retired).
func (t *Token) kekForUnwrap(s pkcs11.SessionHandle, v int) (pkcs11.ObjectHandle, error) {
	h, err := t.findKEK(s, v)
	if err != nil {
		return 0, err
	}
	attrs, err := t.kekAttrs(s, h)
	if err != nil {
		return 0, err
	}
	if _, err := checkKEKAttrs(attrs); err != nil {
		return 0, err
	}
	return h, nil
}

// wrapAndEncrypt wraps dek with kek-v{kekV}, builds the AAD from the wrapped DEK and encrypts pt under a fresh nonce.
func (t *Token) wrapAndEncrypt(s pkcs11.SessionHandle, kek, dek pkcs11.ObjectHandle, kekV int, pt []byte, aadOf AADFunc) (*Sealed, error) {
	wrapped, err := t.ctx.WrapKey(s, wrapMech(), kek, dek)
	if err != nil {
		return nil, err
	}
	aad := aadOf(wrapped)
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	ct, err := t.gcmEncrypt(s, dek, nonce, aad, pt)
	if err != nil {
		return nil, err
	}
	return &Sealed{KEKVersion: kekV, WrappedDEK: wrapped, Nonce: nonce, Ciphertext: ct, AAD: aad}, nil
}

// Seal implements Vault.
func (t *Token) Seal(ctx context.Context, kekVersion int, plaintext []byte, aadOf AADFunc) (*Sealed, error) {
	var out *Sealed
	err := t.withSession(ctx, "vault seal", func(s pkcs11.SessionHandle) error {
		kek, err := t.kekForWrap(s, kekVersion)
		if err != nil {
			return err
		}
		dek, err := t.ctx.GenerateKey(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil)}, dekTemplate())
		if err != nil {
			return err
		}
		defer func() { _ = t.ctx.DestroyObject(s, dek) }()
		out, err = t.wrapAndEncrypt(s, kek, dek, kekVersion, plaintext, aadOf)
		return err
	})
	return out, err
}

// unwrapDEK unwraps o.WrappedDEK with its KEK. A token refusal of the wrapped key (integrity check of RFC 3394) is an authentication failure.
func (t *Token) unwrapDEK(s pkcs11.SessionHandle, kek pkcs11.ObjectHandle, wrapped []byte, extractable bool) (pkcs11.ObjectHandle, error) {
	h, err := t.ctx.UnwrapKey(s, wrapMech(), kek, wrapped, unwrapTemplate(extractable))
	if err != nil {
		var pe pkcs11.Error
		if errors.As(err, &pe) {
			switch uint(pe) {
			case pkcs11.CKR_GENERAL_ERROR, pkcs11.CKR_WRAPPED_KEY_INVALID, pkcs11.CKR_WRAPPED_KEY_LEN_RANGE, pkcs11.CKR_ENCRYPTED_DATA_INVALID, pkcs11.CKR_DATA_INVALID:
				return 0, ErrAuthFailed
			}
		}
		return 0, err
	}
	return h, nil
}

// Unseal implements Vault.
func (t *Token) Unseal(ctx context.Context, o Opened, expectAAD []byte) ([]byte, error) {
	if !bytes.Equal(o.AAD, expectAAD) {
		return nil, ErrAuthFailed
	}
	if err := CheckNonce(o.Nonce); err != nil {
		return nil, err
	}
	var pt []byte
	err := t.withSession(ctx, "vault unseal", func(s pkcs11.SessionHandle) error {
		kek, err := t.kekForUnwrap(s, o.KEKVersion)
		if err != nil {
			return err
		}
		dek, err := t.unwrapDEK(s, kek, o.WrappedDEK, false)
		if err != nil {
			return err
		}
		defer func() { _ = t.ctx.DestroyObject(s, dek) }()
		pt, err = t.gcmDecrypt(s, dek, o.Nonce, o.AAD, o.Ciphertext)
		return err
	})
	if err != nil {
		clear(pt)
		return nil, err
	}
	return pt, nil
}

// Reseal implements Vault.
func (t *Token) Reseal(ctx context.Context, o Opened, expectAAD []byte, toKEK int, newDEK bool, aadOf AADFunc) (*Sealed, error) {
	if !bytes.Equal(o.AAD, expectAAD) {
		return nil, ErrAuthFailed
	}
	if err := CheckNonce(o.Nonce); err != nil {
		return nil, err
	}
	var out *Sealed
	err := t.withSession(ctx, "vault reseal", func(s pkcs11.SessionHandle) error {
		from, err := t.kekForUnwrap(s, o.KEKVersion)
		if err != nil {
			return err
		}
		to, err := t.kekForWrap(s, toKEK)
		if err != nil {
			return err
		}
		old, err := t.unwrapDEK(s, from, o.WrappedDEK, !newDEK) // extractable only when it must be wrapped again
		if err != nil {
			return err
		}
		defer func() { _ = t.ctx.DestroyObject(s, old) }()
		pt, err := t.gcmDecrypt(s, old, o.Nonce, o.AAD, o.Ciphertext)
		if err != nil {
			return err
		}
		defer clear(pt) // the plaintext exists in this process for the length of this call only
		dek := old
		if newDEK {
			if dek, err = t.ctx.GenerateKey(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil)}, dekTemplate()); err != nil {
				return err
			}
			defer func() { _ = t.ctx.DestroyObject(s, dek) }()
		}
		out, err = t.wrapAndEncrypt(s, to, dek, toKEK, pt, aadOf)
		if err == nil && bytes.Equal(out.Nonce, o.Nonce) {
			err = errors.New("reseal produced the nonce of the row it replaces") // 2^-96, but a repeat under one DEK is fatal: refuse
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
