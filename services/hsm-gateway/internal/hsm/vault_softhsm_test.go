//go:build softhsm

package hsm

// The vault against a REAL SoftHSMv2 token (key-custody-hsm.md M3c). Runs in the Docker image of `make test-softhsm`. One process
// holds one PKCS#11 library instance, so scenarios that need another token (init-kek upgrade, a KEK with bad attributes) run this test
// binary again as a child (HSMV_CHILD=...).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/pkcs11"
)

const (
	vUserPIN = "vault-test-user-pin-1"
	vSOPIN   = "vault-test-so-pin-1"
	vToken   = "digiwal-vault-test"
)

var (
	vOnce sync.Once
	vErr  error
	vTok  *Token
	vDir  string
	vConf string
	vPin  string
)

func vModule() string {
	if p := os.Getenv("PKCS11_MODULE"); p != "" {
		return p
	}
	for _, p := range []string{"/usr/lib/softhsm/libsofthsm2.so", "/usr/lib/x86_64-linux-gnu/softhsm/libsofthsm2.so", "/usr/lib/aarch64-linux-gnu/softhsm/libsofthsm2.so"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "libsofthsm2.so"
}

func vCfg(label string) Config {
	return Config{Module: vModule(), TokenLabel: label, KeyLabel: "hot-key", PINFile: vPin, PoolSize: 4}
}

func vSetupEnv() {
	vDir, _ = os.MkdirTemp("", "softhsm-vault-")
	vConf = filepath.Join(vDir, "softhsm2.conf")
	tokens := filepath.Join(vDir, "tokens")
	_ = os.MkdirAll(tokens, 0o700)
	_ = os.WriteFile(vConf, []byte("directories.tokendir = "+tokens+"\nobjectstore.backend = file\nlog.level = ERROR\n"), 0o600)
	_ = os.Setenv("SOFTHSM2_CONF", vConf)
	vPin = filepath.Join(vDir, "user-pin")
	_ = os.WriteFile(vPin, []byte(vUserPIN+"\n"), 0o600)
}

func vault(t *testing.T) *Token {
	t.Helper()
	vOnce.Do(func() {
		vSetupEnv()
		var res *InitResult
		if res, vErr = InitTokenAndKey(vCfg(vToken), vSOPIN); vErr != nil {
			return
		}
		if res.KEKVersion != 1 {
			vErr = fmt.Errorf("init-token generated KEK version %d, want 1", res.KEKVersion)
			return
		}
		vTok, vErr = Open(vCfg(vToken))
	})
	if vErr != nil {
		t.Fatalf("token setup: %v", vErr)
	}
	return vTok
}

func aadFor(tag string) AADFunc {
	return func(w []byte) []byte { return append([]byte(tag+"|"), w[:8]...) }
}

var seed = func() []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = byte(i*3 + 1)
	}
	return b
}()

func opened(s *Sealed) Opened {
	return Opened{KEKVersion: s.KEKVersion, WrappedDEK: append([]byte(nil), s.WrappedDEK...), Nonce: append([]byte(nil), s.Nonce...),
		Ciphertext: append([]byte(nil), s.Ciphertext...), AAD: append([]byte(nil), s.AAD...)}
}

func TestSoftHSM_KEKIsGeneratedInsideTheTokenPrivateNonExtractableAndDistinctFromTheOtherKeys(t *testing.T) {
	tok := vault(t)
	ks, err := tok.KEKs(context.Background())
	if err != nil || len(ks) < 1 || ks[0] != (KEKInfo{Version: 1, Wrap: true}) {
		t.Fatalf("KEKs: %v %v", ks, err)
	}
	// read the attributes back from the token itself, the way the self-test does
	err = tok.withSession(context.Background(), "t", func(s pkcs11.SessionHandle) error {
		h, err := tok.findKEK(s, 1)
		if err != nil {
			return err
		}
		attrs, err := tok.kekAttrs(s, h)
		if err != nil {
			return err
		}
		got := map[uint][]byte{}
		for _, a := range attrs {
			got[a.Type] = a.Value
		}
		for name, c := range map[string]struct {
			attr uint
			want bool
		}{"PRIVATE": {pkcs11.CKA_PRIVATE, true}, "SENSITIVE": {pkcs11.CKA_SENSITIVE, true}, "TOKEN": {pkcs11.CKA_TOKEN, true}, "EXTRACTABLE": {pkcs11.CKA_EXTRACTABLE, false},
			"NEVER_EXTRACTABLE": {pkcs11.CKA_NEVER_EXTRACTABLE, true}, "WRAP": {pkcs11.CKA_WRAP, true}, "UNWRAP": {pkcs11.CKA_UNWRAP, true},
			"ENCRYPT": {pkcs11.CKA_ENCRYPT, false}, "DECRYPT": {pkcs11.CKA_DECRYPT, false}, "SIGN": {pkcs11.CKA_SIGN, false}} {
			if v := got[c.attr]; len(v) == 0 || (v[0] != 0) != c.want {
				t.Errorf("KEK CKA_%s = %v, want %v", name, v, c.want)
			}
		}
		if ulong(got[pkcs11.CKA_VALUE_LEN]) != 32 || ulong(got[pkcs11.CKA_KEY_TYPE]) != pkcs11.CKK_AES {
			t.Errorf("KEK is not AES-256: %v %v", got[pkcs11.CKA_VALUE_LEN], got[pkcs11.CKA_KEY_TYPE])
		}
		// the value cannot be read
		if _, err := tok.ctx.GetAttributeValue(s, h, []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_VALUE, nil)}); err == nil {
			t.Error("CKA_VALUE of the KEK is readable")
		}
		// it cannot wrap itself out of the token (non-extractable)
		if _, err := tok.ctx.WrapKey(s, wrapMech(), h, h); err == nil {
			t.Error("the KEK wrapped itself")
		}
		// it is a different object from the Hot Key and the attestation key
		for _, class := range []uint{pkcs11.CKO_PRIVATE_KEY, pkcs11.CKO_PUBLIC_KEY} {
			hh, n, ferr := tok.findCount(context.Background(), class, "hot-key")
			if ferr != nil || n != 1 || hh == h {
				t.Errorf("hot key object %d (n=%d, err=%v) vs KEK %d", hh, n, ferr, h)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("kek-v1: AES-256 generated inside the token, CKA_PRIVATE/SENSITIVE/TOKEN=true, EXTRACTABLE=false, NEVER_EXTRACTABLE=true, WRAP+UNWRAP only, CKA_VALUE unreadable, cannot wrap itself, distinct from hot-key")
}

func TestSoftHSM_SealUnsealRoundTripAndTheDEKNeverSurvivesTheCall(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	s, err := tok.Seal(ctx, 1, seed, aadFor("user_derivation|sweep|1|1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Nonce) != NonceSize || len(s.WrappedDEK) != 40 || len(s.Ciphertext) != len(seed)+16 || bytes.Contains(s.Ciphertext, seed[:16]) {
		t.Fatalf("sealed shape: nonce %d, wrapped %d (want 40 = RFC 3394 of 32), ct %d", len(s.Nonce), len(s.WrappedDEK), len(s.Ciphertext))
	}
	pt, err := tok.Unseal(ctx, opened(s), s.AAD)
	if err != nil || !bytes.Equal(pt, seed) {
		t.Fatalf("unseal: %v", err)
	}
	// no DEK object is left in the token after the call (session objects are destroyed)
	var n int
	_ = tok.withSession(ctx, "count", func(sh pkcs11.SessionHandle) error {
		_ = tok.ctx.FindObjectsInit(sh, []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY), pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false)})
		objs, _, _ := tok.ctx.FindObjects(sh, 50)
		_ = tok.ctx.FindObjectsFinal(sh)
		n = len(objs)
		return nil
	})
	if n != 0 {
		t.Fatalf("%d session key objects left in the token", n)
	}
	t.Logf("seal/unseal round trip on SoftHSM: DEK generated in the token, wrapped (40 B, CKM_AES_KEY_WRAP), 64 B seed -> %d B AES-256-GCM ciphertext, opened inside the token, 0 DEK objects left", len(s.Ciphertext))
}

func TestSoftHSM_EveryTamperIsAuthenticationFailed(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	if _, err := tok.GenerateKEK(ctx, 2); err != nil {
		t.Fatal(err)
	}
	a, _ := tok.Seal(ctx, 1, seed, aadFor("user_derivation|sweep|1|1"))
	b, _ := tok.Seal(ctx, 1, seed, aadFor("user_derivation|sweep|2|1"))
	cases := map[string]func(o *Opened) []byte{ // each returns the AAD the caller expects
		"aad swapped for another row's (expect = other row's, stored = this row's)": func(o *Opened) []byte { return b.AAD },
		"stored aad replaced by another row's, expect matches":                      func(o *Opened) []byte { o.AAD = b.AAD; return b.AAD },
		"flipped ciphertext bit":      func(o *Opened) []byte { o.Ciphertext[5] ^= 1; return o.AAD },
		"flipped tag bit":             func(o *Opened) []byte { o.Ciphertext[len(o.Ciphertext)-1] ^= 1; return o.AAD },
		"truncated ciphertext":        func(o *Opened) []byte { o.Ciphertext = o.Ciphertext[:len(o.Ciphertext)-1]; return o.AAD },
		"flipped wrapped DEK bit":     func(o *Opened) []byte { o.WrappedDEK[9] ^= 1; return o.AAD },
		"wrapped DEK of another row":  func(o *Opened) []byte { o.WrappedDEK = append([]byte(nil), b.WrappedDEK...); return o.AAD },
		"another KEK version claimed": func(o *Opened) []byte { o.KEKVersion = 2; return o.AAD },
		"another nonce":               func(o *Opened) []byte { o.Nonce = b.Nonce; return o.AAD },
	}
	for name, mut := range cases {
		o := opened(a)
		exp := mut(&o)
		pt, err := tok.Unseal(ctx, o, exp)
		if pt != nil || !errors.Is(err, ErrAuthFailed) {
			t.Errorf("%s: plaintext=%v err=%v, want ErrAuthFailed", name, pt != nil, err)
		}
	}
	if pt, err := tok.Unseal(ctx, opened(a), a.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatalf("the untouched row stopped opening: %v", err)
	}
	t.Logf("%d tamper cases (swapped AAD, flipped ciphertext/tag/wrapped-DEK bit, truncation, other row's DEK, wrong KEK version, wrong nonce) all return ErrAuthFailed; the original row still opens", len(cases))
}

func TestSoftHSM_NonceIsGatewayOwnedRandomUniqueAndZeroIVIsRefused(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	const n = 1500
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		s, err := tok.Seal(ctx, 1, seed, aadFor("u"))
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(s.Nonce)] {
			t.Fatalf("nonce repeated after %d seals", i)
		}
		seen[string(s.Nonce)] = true
		if len(s.Nonce) != 12 || bytes.Equal(s.Nonce, make([]byte, 12)) {
			t.Fatalf("bad nonce %x", s.Nonce)
		}
	}
	// the gateway's own refusal, on every encrypt path
	err := tok.withSession(ctx, "t", func(s pkcs11.SessionHandle) error {
		dek, err := tok.ctx.GenerateKey(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil)}, dekTemplate())
		if err != nil {
			return err
		}
		defer func() { _ = tok.ctx.DestroyObject(s, dek) }()
		for name, iv := range map[string][]byte{"zero 12": make([]byte, 12), "empty": nil, "8 bytes": make([]byte, 8), "16 bytes": bytes.Repeat([]byte{1}, 16)} {
			if _, err := tok.gcmEncrypt(s, dek, iv, []byte("aad"), seed); !errors.Is(err, ErrBadNonce) {
				t.Errorf("%s IV: %v, want ErrBadNonce", name, err)
			}
		}
		// and the token itself would have accepted a zero-length IV (spike M0 (a)): this is why the refusal is ours
		p := pkcs11.NewGCMParams(nil, []byte("aad"), 128)
		defer p.Free()
		if e := tok.ctx.EncryptInit(s, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, p)}, dek); e == nil {
			if _, e = tok.ctx.Encrypt(s, seed); e == nil {
				t.Log("SoftHSM accepted a zero-length GCM IV when asked directly (M0 (a)): the gateway's CheckNonce is the only thing that refuses it")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// a broken randomness source can never reach the token with a bad nonce
	old := newNonce
	defer func() { newNonce = old }()
	newNonce = func() ([]byte, error) { return make([]byte, 12), nil }
	if _, err := tok.Seal(ctx, 1, seed, aadFor("u")); !errors.Is(err, ErrBadNonce) {
		t.Fatalf("a zero nonce from the source: %v", err)
	}
	newNonce = func() ([]byte, error) { return nil, errors.New("entropy exhausted") }
	if _, err := tok.Seal(ctx, 1, seed, aadFor("u")); err == nil {
		t.Fatal("seal succeeded without a nonce")
	}
	// a stored row with a zero nonce is refused on unseal and reseal before the token is asked
	newNonce = old
	s, _ := tok.Seal(ctx, 1, seed, aadFor("u"))
	o := opened(s)
	o.Nonce = make([]byte, 12)
	if _, err := tok.Unseal(ctx, o, o.AAD); !errors.Is(err, ErrBadNonce) {
		t.Fatalf("unseal with a zero nonce: %v", err)
	}
	t.Logf("%d seals on SoftHSM: %d distinct 96-bit nonces; zero, empty, 8 and 16 byte IVs refused (ErrBadNonce); broken nonce source refused", n, len(seen))
}

func TestSoftHSM_ResealRewrapsInsideTheTokenAndRollbackWorks(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	if _, err := tok.GenerateKEK(ctx, 2); err != nil {
		t.Fatal(err)
	}
	a, err := tok.Seal(ctx, 1, seed, aadFor("row1"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := tok.Reseal(ctx, opened(a), a.AAD, 2, false, aadFor("row2"))
	if err != nil {
		t.Fatal(err)
	}
	if r2.KEKVersion != 2 || bytes.Equal(r2.WrappedDEK, a.WrappedDEK) || bytes.Equal(r2.Nonce, a.Nonce) || bytes.Equal(r2.Ciphertext, a.Ciphertext) {
		t.Fatalf("rewrap did not change the wrapper, nonce and ciphertext")
	}
	// two versions are valid during the rotation
	for name, s := range map[string]*Sealed{"v1 row": a, "v2 row": r2} {
		if pt, err := tok.Unseal(ctx, opened(s), s.AAD); err != nil || !bytes.Equal(pt, seed) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// DEK rotation: same KEK, a new DEK (the wrapped bytes differ even though the KEK is the same)
	r3, err := tok.Reseal(ctx, opened(r2), r2.AAD, 2, true, aadFor("row3"))
	if err != nil || r3.KEKVersion != 2 || bytes.Equal(r3.WrappedDEK, r2.WrappedDEK) {
		t.Fatalf("rotate-dek: %v", err)
	}
	if pt, err := tok.Unseal(ctx, opened(r3), r3.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatal(err)
	}
	// a re-run is idempotent in effect: another independent, valid row from the same old row
	r2b, err := tok.Reseal(ctx, opened(a), a.AAD, 2, false, aadFor("row2"))
	if err != nil || bytes.Equal(r2b.Nonce, r2.Nonce) {
		t.Fatalf("second rewrap: %v", err)
	}
	if pt, err := tok.Unseal(ctx, opened(r2b), r2b.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatal(err)
	}
	// a tampered source row is never re-sealed
	bad := opened(a)
	bad.Ciphertext[0] ^= 1
	if _, err := tok.Reseal(ctx, bad, bad.AAD, 2, false, aadFor("x")); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("reseal of a tampered row: %v", err)
	}
	t.Log("rewrap v1->v2 and DEK rotation inside the token: new wrapped DEK, nonce and ciphertext each time; v1 row, v2 row and v3 row all open; a tampered row is refused")
}

func TestSoftHSM_KEKRotationWhileUnsealsRunConcurrently(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	ks, _ := tok.KEKs(ctx)
	cur := ks[len(ks)-1].Version
	next := cur + 1
	if next > MaxKEKVersion {
		t.Skip("out of versions")
	}
	first, err := tok.Seal(ctx, cur, seed, aadFor("rot1"))
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Pointer[Sealed]
	active.Store(first)
	var stop atomic.Bool
	var ops, errs atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				row := active.Load() // "sweep-signer reads status = 'active' per request"
				pt, err := tok.Unseal(ctx, opened(row), row.AAD)
				ops.Add(1)
				if err != nil || !bytes.Equal(pt, seed) {
					errs.Add(1)
					firstErr.CompareAndSwap(nil, fmt.Sprintf("kek %d: %v", row.KEKVersion, err))
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if created, err := tok.GenerateKEK(ctx, next); err != nil || !created {
		t.Fatalf("generate: %v %v", created, err)
	}
	second, err := tok.Reseal(ctx, opened(first), first.AAD, next, false, aadFor("rot2")) // pending row, verified, then flipped
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := tok.Unseal(ctx, opened(second), second.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatalf("verify of the pending row: %v", err)
	}
	active.Store(second) // the flip
	time.Sleep(100 * time.Millisecond)
	// the old KEK becomes unwrap-only while sweeps keep running, and then the old row still opens (rollback is possible)
	if err := tok.RetireKEK(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if pt, err := tok.Unseal(ctx, opened(first), first.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatalf("a retired KEK must still unwrap the old row (rollback): %v", err)
	}
	if _, err := tok.Reseal(ctx, opened(second), second.AAD, cur, false, aadFor("back")); err == nil {
		t.Fatal("re-sealing under a retired KEK was accepted")
	}
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d of %d concurrent unseals failed during the rotation, first: %v", errs.Load(), ops.Load(), firstErr.Load())
	}
	// retention over: destroy the old KEK, the old row is now unreadable, the new one is fine
	if err := tok.DestroyKEK(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := tok.Unseal(ctx, opened(first), first.AAD); !errors.Is(err, ErrNoKEK) {
		t.Fatalf("row under a destroyed KEK: %v, want ErrNoKEK", err)
	}
	if pt, err := tok.Unseal(ctx, opened(second), second.AAD); err != nil || !bytes.Equal(pt, seed) {
		t.Fatal(err)
	}
	t.Logf("KEK rotation kek-v%d -> kek-v%d with 12 goroutines unsealing the active row throughout: %d unseals, %d errors (generate, rewrap, verify, flip, retire, destroy)", cur, next, ops.Load(), errs.Load())
}

func TestSoftHSM_KEKAdminIsIdempotentConsecutiveAndRefusesToStrandData(t *testing.T) {
	tok := vault(t)
	ctx := context.Background()
	ks, _ := tok.KEKs(ctx)
	hi := ks[len(ks)-1].Version
	if created, err := tok.GenerateKEK(ctx, hi); err != nil || created {
		t.Fatalf("generate of an existing version: %v %v", created, err)
	}
	if _, err := tok.GenerateKEK(ctx, hi+2); err == nil {
		t.Fatal("a gap in KEK versions was accepted")
	}
	if err := tok.RetireKEK(ctx, hi); err == nil {
		t.Fatal("the newest KEK was retired")
	}
	if err := tok.DestroyKEK(ctx, hi); err == nil {
		t.Fatal("the newest KEK was destroyed")
	}
	if err := tok.DestroyKEK(ctx, MaxKEKVersion); !errors.Is(err, ErrNoKEK) {
		t.Fatalf("destroy of a missing KEK: %v", err)
	}
	// a wrapping KEK cannot be destroyed without being retired first
	if _, err := tok.GenerateKEK(ctx, hi+1); err != nil {
		t.Fatal(err)
	}
	if err := tok.DestroyKEK(ctx, hi); err == nil {
		t.Fatal("a KEK that still wraps was destroyed")
	}
	if err := tok.RetireKEK(ctx, hi); err != nil {
		t.Fatal(err)
	}
	if err := tok.RetireKEK(ctx, hi); err != nil {
		t.Fatalf("retire is idempotent: %v", err)
	}
	if _, err := tok.Seal(ctx, hi, seed, aadFor("x")); err == nil {
		t.Fatal("sealed under a retired KEK")
	}
}

// ---- child processes: init-kek upgrade and a KEK with bad attributes need a second token

func TestSoftHSMVaultChild(t *testing.T) {
	mode := os.Getenv("HSMV_CHILD")
	if mode == "" {
		t.Skip("child process entry point")
	}
	_ = os.Setenv("SOFTHSM2_CONF", os.Getenv("HSMV_CONF"))
	vPin = os.Getenv("HSMV_PIN")
	label := os.Getenv("HSMV_LABEL")
	fail := func(f string, a ...any) { fmt.Printf("FAIL "+f+"\n", a...); os.Exit(1) }
	switch mode {
	case "init-kek": // a token created before M3c: no KEK. init-kek generates v1, a second run refuses.
		raw := pkcs11.New(vModule())
		if err := raw.Initialize(); err != nil {
			fail("%v", err)
		}
		slots, _ := raw.GetSlotList(true)
		var slot uint
		for _, s := range slots {
			if ti, err := raw.GetTokenInfo(s); err == nil && strings.TrimSpace(ti.Label) == label {
				slot = s
			}
		}
		sh, _ := raw.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
		if err := raw.Login(sh, pkcs11.CKU_USER, vUserPIN); err != nil {
			fail("login %v", err)
		}
		_ = raw.FindObjectsInit(sh, []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY), pkcs11.NewAttribute(pkcs11.CKA_LABEL, "kek-v1")})
		objs, _, _ := raw.FindObjects(sh, 5)
		_ = raw.FindObjectsFinal(sh)
		if len(objs) != 1 {
			fail("expected the KEK init-token made, found %d", len(objs))
		}
		_ = raw.DestroyObject(sh, objs[0]) // now it looks like a pre-M3c token
		_ = raw.Logout(sh)
		_ = raw.CloseSession(sh)
		_ = raw.Finalize()
		raw.Destroy()
		tok, err := Open(vCfg(label))
		if err != nil || tok.HasKEK() {
			fail("open pre-M3c token: err=%v hasKEK=%v", err, tok != nil && tok.HasKEK())
		}
		tok.Close()
		v, err := InitKEK(vCfg(label))
		if err != nil || v != 1 {
			fail("init-kek: %d %v", v, err)
		}
		if _, err := InitKEK(vCfg(label)); err == nil || !strings.Contains(err.Error(), "already exists") {
			fail("second init-kek: %v", err)
		}
		tok, err = Open(vCfg(label))
		if err != nil {
			fail("%v", err)
		}
		defer tok.Close()
		ks, err := tok.KEKs(context.Background())
		if err != nil || len(ks) != 1 || ks[0].Version != 1 || !ks[0].Wrap {
			fail("after init-kek: %v %v", ks, err)
		}
		s, err := tok.Seal(context.Background(), 1, seed, aadFor("u"))
		if err != nil {
			fail("seal after init-kek: %v", err)
		}
		if pt, err := tok.Unseal(context.Background(), opened(s), s.AAD); err != nil || !bytes.Equal(pt, seed) {
			fail("unseal after init-kek: %v", err)
		}
		fmt.Println("OK")
	case "bad-kek": // a KEK that is not private (plaintext in the token files, M0 (d)) is refused
		raw := pkcs11.New(vModule())
		_ = raw.Initialize()
		slots, _ := raw.GetSlotList(true)
		var slot uint
		for _, s := range slots {
			if ti, err := raw.GetTokenInfo(s); err == nil && strings.TrimSpace(ti.Label) == label {
				slot = s
			}
		}
		sh, _ := raw.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
		_ = raw.Login(sh, pkcs11.CKU_USER, vUserPIN)
		tmpl := kekTemplate(2)
		for i, a := range tmpl {
			if a.Type == pkcs11.CKA_PRIVATE {
				tmpl[i] = pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, false)
			}
		}
		if _, err := raw.GenerateKey(sh, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil)}, tmpl); err != nil {
			fail("generate bad kek: %v", err)
		}
		_ = raw.Logout(sh)
		_ = raw.CloseSession(sh)
		_ = raw.Finalize()
		raw.Destroy()
		tok, err := Open(vCfg(label))
		if err != nil {
			fail("open: %v", err)
		}
		defer tok.Close()
		_, err = tok.KEKs(context.Background())
		var he *Error
		if !errors.As(err, &he) || he.Code != CodeKeyAttrs || !strings.Contains(err.Error(), "CKA_PRIVATE") {
			fail("KEKs with a non-private KEK: %v", err)
		}
		// the bad KEK itself is never used: sealing under it, or unsealing a row that names it, is refused
		if _, err := tok.Seal(context.Background(), 2, seed, aadFor("u")); err == nil {
			fail("sealed under the non-private KEK")
		}
		if _, err := tok.Unseal(context.Background(), Opened{KEKVersion: 2, WrappedDEK: make([]byte, 40), Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: make([]byte, 80), AAD: []byte("a")}, []byte("a")); err == nil || errors.Is(err, ErrAuthFailed) {
			fail("unseal under the non-private KEK: %v", err)
		}
		fmt.Println("OK")
	}
}

func vChild(t *testing.T, mode, label string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSoftHSMVaultChild$", "-test.v")
	cmd.Env = append(os.Environ(), "HSMV_CHILD="+mode, "HSMV_CONF="+vConf, "HSMV_PIN="+vPin, "HSMV_LABEL="+label)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "\nOK\n") {
		t.Fatalf("child %s: %v\n%s", mode, err, out)
	}
	return string(out)
}

func TestSoftHSM_InitKEKUpgradesATokenFromBeforeM3cAndRefusesADuplicate(t *testing.T) {
	vault(t) // sets up SOFTHSM2_CONF and the first token
	// a second token in the same directory, created by init-token (which makes the KEK) and then stripped of it by the child
	cfg := vCfg("pre-m3c")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSoftHSMVaultInit$")
	cmd.Env = append(os.Environ(), "HSMV_INIT=pre-m3c", "HSMV_CONF="+vConf, "HSMV_PIN="+vPin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating the second token: %v\n%s", err, out)
	}
	_ = cfg
	vChild(t, "init-kek", "pre-m3c")
	t.Log("init-kek on a token without a KEK: generated kek-v1, the token opens with HasKEK, seal/unseal work; a second init-kek refuses (\"already exists\")")
}

func TestSoftHSMVaultInit(t *testing.T) {
	label := os.Getenv("HSMV_INIT")
	if label == "" {
		t.Skip("child process entry point")
	}
	_ = os.Setenv("SOFTHSM2_CONF", os.Getenv("HSMV_CONF"))
	vPin = os.Getenv("HSMV_PIN")
	if _, err := InitTokenAndKey(vCfg(label), vSOPIN); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
}

func TestSoftHSM_AKEKWithBadAttributesIsRefused(t *testing.T) {
	vault(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSoftHSMVaultInit$")
	cmd.Env = append(os.Environ(), "HSMV_INIT=bad-kek", "HSMV_CONF="+vConf, "HSMV_PIN="+vPin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating the token: %v\n%s", err, out)
	}
	vChild(t, "bad-kek", "bad-kek")
}
