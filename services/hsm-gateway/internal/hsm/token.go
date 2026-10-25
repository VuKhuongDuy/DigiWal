package hsm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/miekg/pkcs11"
)

// secp256k1OID is the DER of OID 1.3.132.0.10, the CKA_EC_PARAMS of the Hot Key.
var secp256k1OID = []byte{0x06, 0x05, 0x2B, 0x81, 0x04, 0x00, 0x0A}

// Config locates the token and key.
type Config struct {
	Module     string // path to the PKCS#11 library
	TokenLabel string
	KeyLabel   string
	// AttestKeyLabel is the label of the xpub attestation key (a P-256 key, never the Hot Key). Empty means
	// DefaultAttestKeyLabel. A token without that key still opens (attestation is then unavailable); a key that
	// is there but has the wrong attributes is refused like the Hot Key.
	AttestKeyLabel string
	PINFile        string // the user PIN is read from this file only: never env, never flags, never logged
	PoolSize       int
	Guard          *PINGuard
}

// DefaultAttestKeyLabel is the label of the attestation key pair.
const DefaultAttestKeyLabel = "attest-key"

// p256OID is the DER of OID 1.2.840.10045.3.1.7, the CKA_EC_PARAMS of the attestation key.
var p256OID = []byte{0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07}

func (c Config) attestLabel() string {
	if c.AttestKeyLabel == "" {
		return DefaultAttestKeyLabel
	}
	return c.AttestKeyLabel
}

// Backend is what the gateway needs from a key holder. The only implementation shipped is the PKCS#11
// Token: there is no software backend in this service, and an HSM failure never falls back.
type Backend interface {
	// Address is the Ethereum address of the key as derived from the token's CKA_EC_POINT.
	Address() common.Address
	// PublicKey is the 65-byte uncompressed public key.
	PublicKey() []byte
	// SignDigest returns the raw 64-byte r||s over a 32-byte digest.
	SignDigest(ctx context.Context, digest []byte) ([]byte, error)
	// CheckKey re-reads the key's attributes and fails unless they are the required ones.
	CheckKey(ctx context.Context) error
	Close()
}

// Token is the PKCS#11 backend.
type Token struct {
	ctx    *pkcs11.Ctx
	pool   *Pool
	priv   pkcs11.ObjectHandle
	pub    pkcs11.ObjectHandle
	addr   common.Address
	pubKey []byte // 65-byte uncompressed
	slot   uint

	// attestation key (optional): P-256, a different object from the Hot Key
	attPriv   pkcs11.ObjectHandle
	attPub    []byte // 65-byte uncompressed P-256 point, nil when the token has no attestation key
	hasAttest bool
	closed    atomic.Bool
}

// ReadPIN reads the user PIN from file, trimming only the trailing newline.
func ReadPIN(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading PIN file: %w", err)
	}
	pin := strings.TrimRight(string(b), "\r\n")
	if pin == "" {
		return "", errors.New("PIN file is empty")
	}
	return pin, nil
}

// findSlot returns the slot of the initialised token with label.
func findSlot(ctx *pkcs11.Ctx, label string) (uint, error) {
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		return 0, Map("GetSlotList", err)
	}
	for _, s := range slots {
		ti, err := ctx.GetTokenInfo(s)
		if err != nil {
			continue
		}
		if strings.TrimSpace(ti.Label) == label && ti.Flags&pkcs11.CKF_TOKEN_INITIALIZED != 0 {
			return s, nil
		}
	}
	return 0, &Error{Code: CodeTokenMissing, Op: "find token", Cause: fmt.Errorf("no initialised token labelled %q", label)}
}

// Open initialises the library, logs in ONCE as the user (a second C_Login per session returns
// CKR_USER_ALREADY_LOGGED_IN, and login state is token-wide), opens the session pool and locates and
// checks the Hot Key. A rejected PIN is recorded in the PIN guard and returned as ErrPINIncorrect.
func Open(cfg Config) (*Token, error) {
	if err := cfg.Guard.Allow(); err != nil {
		return nil, err
	}
	pin, err := ReadPIN(cfg.PINFile)
	if err != nil {
		return nil, err
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 4
	}
	ctx := pkcs11.New(cfg.Module)
	if ctx == nil {
		return nil, &Error{Code: CodeUnavailable, Op: "load module", Cause: fmt.Errorf("cannot load %s", cfg.Module)}
	}
	if err := ctx.Initialize(); err != nil {
		ctx.Destroy()
		return nil, Map("Initialize", err)
	}
	t := &Token{ctx: ctx}
	fail := func(err error) (*Token, error) { t.Close(); return nil, err }

	if t.slot, err = findSlot(ctx, cfg.TokenLabel); err != nil {
		return fail(err)
	}
	openSess := func() (handle, error) {
		h, err := ctx.OpenSession(t.slot, pkcs11.CKF_SERIAL_SESSION)
		if err != nil {
			return 0, Map("OpenSession", err)
		}
		return handle(h), nil
	}
	closeSess := func(h handle) { _ = ctx.CloseSession(pkcs11.SessionHandle(h)) }

	// The login session is opened first and stays in the pool; the login is token-wide.
	first, err := openSess()
	if err != nil {
		return fail(err)
	}
	switch lerr := ctx.Login(pkcs11.SessionHandle(first), pkcs11.CKU_USER, pin); {
	case lerr == nil:
	case errors.Is(lerr, pkcs11.Error(pkcs11.CKR_USER_ALREADY_LOGGED_IN)):
	case errors.Is(lerr, pkcs11.Error(pkcs11.CKR_PIN_INCORRECT)), errors.Is(lerr, pkcs11.Error(pkcs11.CKR_PIN_LOCKED)):
		closeSess(first)
		_ = cfg.Guard.RecordFailure()
		log.Printf("ALERT hsm pin_rejected recent_failures=%d: not retrying", cfg.Guard.Failures())
		return fail(ErrPINIncorrect)
	default:
		closeSess(first)
		return fail(Map("Login", lerr))
	}
	_ = cfg.Guard.RecordSuccess()

	opened := 1
	t.pool, err = newPool(cfg.PoolSize, func() (handle, error) {
		if opened == 1 {
			opened++
			return first, nil
		}
		return openSess()
	}, closeSess)
	if err != nil {
		return fail(err)
	}
	if cfg.attestLabel() == cfg.KeyLabel {
		return fail(&Error{Code: CodeKeyAttrs, Op: "config", Cause: errors.New("the attestation key label equals the Hot Key label: they must be different keys")})
	}
	if err := t.locate(context.Background(), cfg.KeyLabel); err != nil {
		return fail(err)
	}
	if err := t.CheckKey(context.Background()); err != nil {
		return fail(err)
	}
	if err := t.locateAttest(context.Background(), cfg.attestLabel()); err != nil {
		return fail(err)
	}
	return t, nil
}

// withSession runs fn on a leased session, re-opening it once if the handle was dead.
func (t *Token) withSession(ctx context.Context, op string, fn func(s pkcs11.SessionHandle) error) error {
	if t.closed.Load() {
		return &Error{Code: CodeUnavailable, Op: op, Cause: errors.New("token closed")}
	}
	l, err := t.pool.Acquire(ctx)
	if err != nil {
		return &Error{Code: CodeUnavailable, Op: op, Cause: err}
	}
	defer l.Release()
	run := func() error { return l.Do(func(h handle) error { return fn(pkcs11.SessionHandle(h)) }) }
	err = run()
	if err != nil && SessionBroken(err) {
		if rerr := l.Reopen(); rerr != nil {
			return Map(op, rerr)
		}
		err = run()
	}
	if err != nil {
		// A failed operation may leave one active on the session; never reuse it as is.
		_ = l.Reopen()
		return Map(op, err)
	}
	return nil
}

func (t *Token) find(ctx context.Context, class uint, label string) (pkcs11.ObjectHandle, error) {
	h, n, err := t.findCount(ctx, class, label)
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, &Error{Code: CodeKeyMissing, Op: "find key", Cause: fmt.Errorf("%d objects of class %#x labelled %q (want exactly 1)", n, class, label)}
	}
	return h, nil
}

// findCount returns the first object (when any) and how many objects of class carry label.
func (t *Token) findCount(ctx context.Context, class uint, label string) (pkcs11.ObjectHandle, int, error) {
	var out pkcs11.ObjectHandle
	var count int
	err := t.withSession(ctx, "find key", func(s pkcs11.SessionHandle) error {
		tmpl := []*pkcs11.Attribute{
			pkcs11.NewAttribute(pkcs11.CKA_CLASS, class),
			pkcs11.NewAttribute(pkcs11.CKA_LABEL, label),
		}
		if err := t.ctx.FindObjectsInit(s, tmpl); err != nil {
			return err
		}
		objs, _, err := t.ctx.FindObjects(s, 3)
		_ = t.ctx.FindObjectsFinal(s)
		if err != nil {
			return err
		}
		count = len(objs)
		if count > 0 {
			out = objs[0]
		}
		return nil
	})
	return out, count, err
}

func (t *Token) locate(ctx context.Context, label string) error {
	var err error
	if t.priv, err = t.find(ctx, pkcs11.CKO_PRIVATE_KEY, label); err != nil {
		return err
	}
	if t.pub, err = t.find(ctx, pkcs11.CKO_PUBLIC_KEY, label); err != nil {
		return err
	}
	var attrs []*pkcs11.Attribute
	if err := t.withSession(ctx, "read public key", func(s pkcs11.SessionHandle) error {
		var e error
		attrs, e = t.ctx.GetAttributeValue(s, t.pub, []*pkcs11.Attribute{
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
	if !bytes.Equal(params, secp256k1OID) {
		return &Error{Code: CodeKeyAttrs, Op: "read public key", Cause: errors.New("key is not on secp256k1")}
	}
	raw, err := ParseECPoint(point)
	if err != nil {
		return &Error{Code: CodeKeyAttrs, Op: "read public key", Cause: err}
	}
	pk, err := crypto.UnmarshalPubkey(raw)
	if err != nil {
		return &Error{Code: CodeKeyAttrs, Op: "read public key", Cause: err}
	}
	t.pubKey, t.addr = raw, crypto.PubkeyToAddress(*pk)
	return nil
}

// ParseECPoint accepts CKA_EC_POINT as returned by tokens: a DER OCTET STRING wrapping the
// 65-byte uncompressed point, or the bare point.
func ParseECPoint(b []byte) ([]byte, error) {
	if len(b) == 67 && b[0] == 0x04 && b[1] == 0x41 {
		b = b[2:]
	}
	if len(b) != 65 || b[0] != 0x04 {
		return nil, fmt.Errorf("unexpected CKA_EC_POINT encoding (%d bytes)", len(b))
	}
	return b, nil
}

// Address implements Backend.
func (t *Token) Address() common.Address { return t.addr }

// PublicKey is the 65-byte uncompressed public key.
func (t *Token) PublicKey() []byte { return append([]byte(nil), t.pubKey...) }

// CheckKey reads the private key's attributes back and refuses unless the key is private
// (M0 spike (d): CKA_PRIVATE=false keys sit in plaintext in the token files), sensitive, never
// extractable, and a signing key.
func (t *Token) CheckKey(ctx context.Context) error {
	var attrs []*pkcs11.Attribute
	err := t.withSession(ctx, "read key attributes", func(s pkcs11.SessionHandle) error {
		var e error
		attrs, e = t.ctx.GetAttributeValue(s, t.priv, []*pkcs11.Attribute{
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

func checkAttrs(attrs []*pkcs11.Attribute) error {
	got := map[uint][]byte{}
	for _, a := range attrs {
		got[a.Type] = a.Value
	}
	isTrue := func(t uint) bool { v := got[t]; return len(v) > 0 && v[0] != 0 }
	bad := func(why string) error {
		return &Error{Code: CodeKeyAttrs, Op: "check key attributes", Cause: errors.New(why)}
	}
	switch {
	case !isTrue(pkcs11.CKA_PRIVATE):
		return bad("CKA_PRIVATE is not true (the key would be stored in plaintext in the token files)")
	case !isTrue(pkcs11.CKA_SENSITIVE):
		return bad("CKA_SENSITIVE is not true")
	case isTrue(pkcs11.CKA_EXTRACTABLE):
		return bad("CKA_EXTRACTABLE is true")
	case !isTrue(pkcs11.CKA_NEVER_EXTRACTABLE):
		return bad("CKA_NEVER_EXTRACTABLE is not true (the key was extractable at some point)")
	case !isTrue(pkcs11.CKA_SIGN):
		return bad("CKA_SIGN is not true")
	}
	if ulong(got[pkcs11.CKA_KEY_TYPE]) != pkcs11.CKK_EC {
		return bad("key type is not EC")
	}
	return nil
}

// ulong decodes a CK_ULONG attribute value (host byte order, little-endian on every supported platform).
func ulong(b []byte) uint {
	var buf [8]byte
	copy(buf[:], b)
	return uint(binary.LittleEndian.Uint64(buf[:]))
}

// SignDigest signs a 32-byte digest with CKM_ECDSA on a leased session and returns r||s.
func (t *Token) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if len(digest) != 32 {
		return nil, &Error{Code: CodeBadOutput, Op: "sign", Cause: fmt.Errorf("digest is %d bytes", len(digest))}
	}
	var rs []byte
	err := t.withSession(ctx, "sign", func(s pkcs11.SessionHandle) error {
		if err := t.ctx.SignInit(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_ECDSA, nil)}, t.priv); err != nil {
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
		return nil, &Error{Code: CodeBadOutput, Op: "sign", Cause: fmt.Errorf("token returned %d bytes, want 64", len(rs))}
	}
	return rs, nil
}

// Close closes sessions and finalises the library.
func (t *Token) Close() {
	if !t.closed.CompareAndSwap(false, true) {
		return
	}
	if t.pool != nil {
		t.pool.Close()
	}
	if t.ctx != nil {
		_ = t.ctx.Finalize()
		t.ctx.Destroy()
	}
}
