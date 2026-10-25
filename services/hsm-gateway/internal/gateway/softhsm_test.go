//go:build softhsm

package gateway

// Integration tests against a REAL SoftHSMv2 token. They run inside the Docker image built by
// `make test-softhsm` (the host has no softhsm). One process = one PKCS#11 library instance, so the
// scenarios that need a second Open (wrong PIN, bad key attributes, crash) run the same test binary
// as a child process (HSMGW_HELPER=open|1).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/miekg/pkcs11"

	"digiwal/services/hsm-gateway/internal/attest"
	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/sigutil"
	"digiwal/services/hsm-gateway/internal/vault"
)

const (
	testUserPIN = "test-user-pin-4711"
	testSOPIN   = "test-so-pin-0815"
	tokenLabel  = "digiwal-hot"
	keyLabel    = "hot-key"
)

var (
	setupOnce sync.Once
	setupErr  error
	tokDir    string
	pinFile   string
	realTok   *hsm.Token
	initRes   *hsm.InitResult
)

func modulepath() string {
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

func cfg(guard *hsm.PINGuard) hsm.Config {
	return hsm.Config{Module: modulepath(), TokenLabel: tokenLabel, KeyLabel: keyLabel, PINFile: pinFile, PoolSize: 4, Guard: guard}
}

// token returns the process-wide initialised token (init-token path, then the normal Open path).
func token(t *testing.T) *hsm.Token {
	t.Helper()
	setupOnce.Do(func() {
		tokDir, _ = os.MkdirTemp("", "softhsm-tokens-")
		conf := filepath.Join(tokDir, "softhsm2.conf")
		tokens := filepath.Join(tokDir, "tokens")
		_ = os.MkdirAll(tokens, 0o700)
		_ = os.WriteFile(conf, []byte("directories.tokendir = "+tokens+"\nobjectstore.backend = file\nlog.level = ERROR\n"), 0o600)
		_ = os.Setenv("SOFTHSM2_CONF", conf)
		pinFile = filepath.Join(tokDir, "user-pin")
		_ = os.WriteFile(pinFile, []byte(testUserPIN+"\n"), 0o600)
		if initRes, setupErr = hsm.InitTokenAndKey(cfg(nil), testSOPIN); setupErr != nil {
			return
		}
		if setupErr = makeBadKeys(); setupErr != nil {
			return
		}
		realTok, setupErr = hsm.Open(cfg(nil))
	})
	if setupErr != nil {
		t.Fatalf("token setup: %v", setupErr)
	}
	return realTok
}

// makeBadKeys creates two keys that violate the Hot Key requirements, to prove Open refuses them.
func makeBadKeys() error {
	ctx := pkcs11.New(modulepath())
	if err := ctx.Initialize(); err != nil {
		return err
	}
	defer func() { _ = ctx.Finalize(); ctx.Destroy() }()
	slots, _ := ctx.GetSlotList(true)
	var slot uint
	for _, s := range slots {
		if ti, err := ctx.GetTokenInfo(s); err == nil && strings.TrimSpace(ti.Label) == tokenLabel {
			slot = s
		}
	}
	sh, err := ctx.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		return err
	}
	if err := ctx.Login(sh, pkcs11.CKU_USER, testUserPIN); err != nil {
		return err
	}
	oid := []byte{0x06, 0x05, 0x2B, 0x81, 0x04, 0x00, 0x0A}
	var genWith func(label string, oid []byte, private, extractable bool) error
	p256 := []byte{0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07}
	gen := func(label string, private, extractable bool) error { return genWith(label, oid, private, extractable) }
	genWith = func(label string, oid []byte, private, extractable bool) error {
		pub := []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_EC_PARAMS, oid), pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
			pkcs11.NewAttribute(pkcs11.CKA_VERIFY, true), pkcs11.NewAttribute(pkcs11.CKA_LABEL, label)}
		priv := []*pkcs11.Attribute{pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true), pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, private),
			pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, !extractable), pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, extractable),
			pkcs11.NewAttribute(pkcs11.CKA_SIGN, true), pkcs11.NewAttribute(pkcs11.CKA_LABEL, label)}
		_, _, err := ctx.GenerateKeyPair(sh, []*pkcs11.Mechanism{pkcs11.NewMechanism(pkcs11.CKM_EC_KEY_PAIR_GEN, nil)}, pub, priv)
		return err
	}
	if err := gen("bad-not-private", false, false); err != nil {
		return err
	}
	if err := genWith("bad-att-not-private", p256, false, false); err != nil {
		return err
	}
	if err := genWith("bad-att-extractable", p256, true, true); err != nil {
		return err
	}
	if err := genWith("bad-att-secp256k1", oid, true, false); err != nil { // attestation label on the wrong curve
		return err
	}
	return gen("bad-extractable", true, true)
}

// openHelperMain: child process that only calls hsm.Open with the env config and reports the outcome.
func openHelperMain() {
	_ = os.Setenv("SOFTHSM2_CONF", os.Getenv("HSMGW_CONF"))
	var guard *hsm.PINGuard
	if g := os.Getenv("HSMGW_GUARD"); g != "" {
		guard = hsm.NewPINGuard(g, 3, time.Hour)
	}
	c := hsm.Config{Module: modulepath(), TokenLabel: tokenLabel, KeyLabel: os.Getenv("HSMGW_LABEL"), AttestKeyLabel: os.Getenv("HSMGW_ATTEST_LABEL"),
		PINFile: os.Getenv("HSMGW_PIN"), PoolSize: 4, Guard: guard}
	if os.Getenv("HSMGW_ATTEST_INIT") != "" { // child: the pre-M3a upgrade step (init-attest-key) under another label
		pk, err := hsm.InitAttestKey(c)
		if err != nil {
			fmt.Println("REFUSED:", err)
			os.Exit(1)
		}
		fmt.Printf("ok attest_public_key=%x\n", pk)
		return
	}
	tok, err := hsm.Open(c)
	switch {
	case err == nil:
		fmt.Printf("ok address=%s attest=%v\n", tok.Address(), tok.HasAttestKey())
		tok.Close()
	case errorsIs(err, hsm.ErrPINIncorrect):
		fmt.Println("pin_incorrect")
		os.Exit(3)
	case errorsIs(err, hsm.ErrPINGuardTripped):
		fmt.Println("guard_tripped")
		os.Exit(3)
	default:
		code := "other"
		if he, ok := err.(*hsm.Error); ok {
			code = he.Code
		}
		fmt.Printf("error code=%s detail=%v\n", code, err)
		os.Exit(1)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// openRealToken is used by the crash helper child (HSMGW_SOFTHSM=1).
func openRealToken() hsm.Backend {
	_ = os.Setenv("SOFTHSM2_CONF", os.Getenv("HSMGW_CONF"))
	tok, err := hsm.Open(hsm.Config{Module: modulepath(), TokenLabel: tokenLabel, KeyLabel: keyLabel, PINFile: os.Getenv("HSMGW_PIN"), PoolSize: 4})
	if err != nil {
		panic(err)
	}
	return tok
}

// runChild runs the open/attest-init helper in a child process with extra env and returns stdout and the exit code.
func runChild(t *testing.T, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{"HSMGW_HELPER=open", "HSMGW_CONF=" + os.Getenv("SOFTHSM2_CONF"), "HSMGW_PIN=" + pinFile, "HSMGW_LABEL=" + keyLabel}, env...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), code
}

func runOpen(t *testing.T, label, pin, guard string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HSMGW_HELPER=open", "HSMGW_CONF="+os.Getenv("SOFTHSM2_CONF"), "HSMGW_LABEL="+label, "HSMGW_PIN="+pin, "HSMGW_GUARD="+guard)
	cmd.Stderr = os.Stderr // the child's ALERT log lines
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), code
}

// ---- tests

func TestSoftHSM_InitOpenAndSelfTest(t *testing.T) {
	tok := token(t)
	if !initRes.NewToken || tok.Address() != initRes.Address {
		t.Fatalf("init address %s vs token %s", initRes.Address, tok.Address())
	}
	// the key attributes the self-test enforces, read back from the real token
	if err := tok.CheckKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, tok, 1337)
	if err := f.svc.SelfTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("token initialised, Hot Key generated inside it: address %s, CheckKey (private, sensitive, non-extractable, EC) passed, self-test (nonce 2^64-1) passed", tok.Address())

	// init-token refuses to create a second Hot Key or re-initialise (it needs a second Initialize in this process,
	// so run it as a child): covered by the helper below.
}

func TestSoftHSM_KeysWithWrongAttributesAreRefused(t *testing.T) {
	token(t)
	for label, why := range map[string]string{"bad-not-private": "CKA_PRIVATE", "bad-extractable": "CKA_"} {
		out, code := runOpen(t, label, pinFile, "")
		if code == 0 || !strings.Contains(out, "code="+hsm.CodeKeyAttrs) || !strings.Contains(out, why) {
			t.Errorf("label %s: exit %d output %q, want refusal hsm_key_attributes mentioning %s", label, code, out, why)
		} else {
			t.Logf("%s refused at startup: %s", label, out)
		}
	}
	if out, code := runOpen(t, "no-such-key", pinFile, ""); code == 0 || !strings.Contains(out, hsm.CodeKeyMissing) {
		t.Errorf("missing key: %d %q", code, out)
	}
	if out, code := runOpen(t, keyLabel, pinFile, ""); code != 0 || !strings.HasPrefix(out, "ok address=") {
		t.Errorf("good key from a second process: %d %q", code, out)
	}
}

func TestSoftHSM_WrongPINIsNeverRetriedAndTheGuardLimitsAttempts(t *testing.T) {
	token(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-pin")
	_ = os.WriteFile(bad, []byte("definitely-wrong\n"), 0o600)
	guard := filepath.Join(dir, "state", "pin-guard.json")
	for i := 1; i <= 3; i++ {
		out, code := runOpen(t, keyLabel, bad, guard)
		if code != 3 || out != "pin_incorrect" {
			t.Fatalf("attempt %d: exit %d %q, want pin_incorrect exit 3", i, code, out)
		}
	}
	out, code := runOpen(t, keyLabel, bad, guard)
	if code != 3 || out != "guard_tripped" {
		t.Fatalf("4th attempt: exit %d %q, want guard_tripped (the token must not even be tried)", code, out)
	}
	// even the correct PIN is refused while tripped; an operator clears the file after investigating
	if out, code := runOpen(t, keyLabel, pinFile, guard); out != "guard_tripped" {
		t.Fatalf("correct PIN during trip: %d %q", code, out)
	}
	_ = os.Remove(guard)
	if out, code := runOpen(t, keyLabel, pinFile, guard); code != 0 {
		t.Fatalf("correct PIN after the operator cleared the guard: %d %q", code, out)
	}
	if b, _ := os.ReadFile(guard); strings.Contains(string(b), testUserPIN) || strings.Contains(string(b), "definitely-wrong") {
		t.Fatal("PIN found in the guard state file")
	}
	t.Log("3 wrong PINs -> pin_incorrect each (process exits 3, no retry); 4th start refused by the guard without touching the token; correct PIN works again after the guard is cleared")
}

func TestSoftHSM_PropertyTenThousandDigestsRecoverTheExpectedAddress(t *testing.T) {
	tok := token(t)
	const total, workers = 10000, 16
	var done, high atomic.Int64
	var wg sync.WaitGroup
	errc := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Add(1) <= total {
				d := make([]byte, 32)
				_, _ = rand.Read(d)
				rs, err := tok.SignDigest(context.Background(), d)
				if err != nil {
					errc <- err
					return
				}
				if new(big.Int).SetBytes(rs[32:]).Cmp(new(big.Int).Rsh(crypto.S256().Params().N, 1)) > 0 {
					high.Add(1)
				}
				sig, err := sigutil.Finalize(d, rs, tok.Address())
				if err != nil {
					errc <- fmt.Errorf("digest %x: %w", d, err)
					return
				}
				if !sigutil.IsLowS(sig) {
					errc <- fmt.Errorf("high s returned")
					return
				}
				pub, err := crypto.SigToPub(d, sig)
				if err != nil || crypto.PubkeyToAddress(*pub) != tok.Address() {
					errc <- fmt.Errorf("digest %x does not recover the Hot address", d)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	t.Logf("%d real-token signatures over random digests (%d goroutines, pool of 4): all recover the Hot address; %d raw signatures (%.1f%%) had a high s and were normalised", total, workers, high.Load(), 100*float64(high.Load())/total)
	if high.Load() == 0 {
		t.Fatal("no high-s raw signature seen: the token output is not exercising normalisation")
	}
}

func TestSoftHSM_ConcurrentSignsThroughTheServiceDoNotCrashTheLibrary(t *testing.T) {
	tok := token(t)
	f := newFixture(t, tok, 1337)
	var nonce atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := f.svc.Sign(context.Background(), "wallet-service", payout(1337, nonce.Add(1), user, 1)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if _, _, err := audit.Verify(f.logPath, audit.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("16 goroutines x 50 service-level signs over a pool of 4 real sessions: no error, no SIGSEGV, audit chain verifies (head seq %d)", func() uint64 { s, _ := f.log.Head(); return s }())
}

func TestSoftHSM_RealEVMAcceptsAndMinesSignaturesFromTheHSM(t *testing.T) {
	tok := token(t)
	RunChainTest(t, tok, func(t *testing.T, chainID uint64) *Service { return newFixture(t, tok, chainID).svc })
	t.Log("simulated EVM accepted and mined: 3 payouts, a Fee Bump replacement and a Cancel replacement, all signed by the SoftHSM token")
}

func TestSoftHSM_KillMidSignAgainstTheRealToken(t *testing.T) {
	tok := token(t)
	_ = tok
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")
	env := []string{"HSMGW_SOFTHSM=1", "HSMGW_CONF=" + os.Getenv("SOFTHSM2_CONF"), "HSMGW_PIN=" + pinFile}
	h := startHelper(t, "", logPath, 3, env...)
	for i := 0; i < 5; i++ {
		_ = h.in.Encode(map[string]any{"id": i, "nonce": i, "value": 1})
		time.Sleep(400 * time.Millisecond)
	}
	_ = h.inW.Close()
	h.collect()
	if len(h.sigs) != 2 {
		t.Fatalf("%d signatures (errs %v), want 2", len(h.sigs), h.errs)
	}
	authorized := assertNoSignatureWithoutAuditLine(t, logPath, h.sigs)
	if authorized != 3 {
		t.Fatalf("authorized audit lines: %d, want 3", authorized)
	}
	// a restarted gateway on the same token and log signs the killed nonce again
	h2 := startHelper(t, "", logPath, 0, env...)
	_ = h2.in.Encode(map[string]any{"id": 2, "nonce": 2, "value": 1})
	_ = h2.inW.Close()
	h2.collect()
	if _, ok := h2.sigs[2]; !ok {
		t.Fatalf("retry after restart: %v", h2.errs)
	}
	assertNoSignatureWithoutAuditLine(t, logPath, h2.sigs)
	t.Logf("real token, SIGKILL inside the 3rd HSM call: %d signatures returned, %d authorized audit lines; restart signs the killed nonce again", len(h.sigs), authorized)
}

func TestSoftHSM_InitTokenRefusesASecondHotKey(t *testing.T) {
	token(t)
	// a child process runs init against the existing token: it must refuse
	cmd := exec.Command(os.Args[0], "-test.run=^TestSoftHSM_InitChild$")
	cmd.Env = append(os.Environ(), "HSMGW_INIT_CHILD=1", "HSMGW_CONF="+os.Getenv("SOFTHSM2_CONF"), "HSMGW_PIN="+pinFile)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "REFUSED:") {
		t.Fatalf("init against an existing token: err=%v out=%s", err, out)
	}
	t.Logf("second init-token: %s", strings.TrimSpace(string(out[strings.Index(string(out), "REFUSED:"):])))
}

func TestSoftHSM_InitChild(t *testing.T) {
	if os.Getenv("HSMGW_INIT_CHILD") != "1" {
		t.Skip("child of TestSoftHSM_InitTokenRefusesASecondHotKey")
	}
	_ = os.Setenv("SOFTHSM2_CONF", os.Getenv("HSMGW_CONF"))
	pinFile = os.Getenv("HSMGW_PIN")
	_, err := hsm.InitTokenAndKey(cfg(nil), testSOPIN)
	if err == nil {
		fmt.Println("INIT SUCCEEDED (bad)")
		os.Exit(1)
	}
	fmt.Println("REFUSED:", err)
}

// ---- M3a: the attestation key (key-custody-hsm.md invariant 10)

func attestFixture(t *testing.T) (*hsm.Token, *fixture) {
	t.Helper()
	tok := token(t)
	f := newFixture(t, tok, 1337)
	f.svc.WithAttestor(tok)
	return tok, f
}

func TestSoftHSM_AttestKeyIsGeneratedInsideTheTokenAndIsNotTheHotKey(t *testing.T) {
	tok := token(t)
	if len(initRes.AttestPublicKey) != 65 || !tok.HasAttestKey() || string(tok.AttestPublicKey()) != string(initRes.AttestPublicKey) {
		t.Fatalf("init-token's attestation key %x vs token %x (has=%v)", initRes.AttestPublicKey, tok.AttestPublicKey(), tok.HasAttestKey())
	}
	if string(tok.AttestPublicKey()) == string(tok.PublicKey()) {
		t.Fatal("the attestation key is the Hot Key")
	}
	if err := tok.CheckAttestKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the same attribute read-back the self-test does for the Hot Key: private, sensitive, non-extractable, never-extractable
	t.Logf("attestation key (P-256) generated inside the token, CheckAttestKey (private, sensitive, non-extractable, never-extractable, EC sign) passed; distinct from the Hot Key")
}

func TestSoftHSM_AttestSignsTheStatementThroughTheServiceAndAudits(t *testing.T) {
	tok, f := attestFixture(t)
	seed := make([]byte, 64)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	m, _ := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	for _, i := range []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart} {
		m, _ = m.Derive(i)
	}
	pub, _ := m.Neuter()
	q := &attest.Request{Chain: attest.ChainEVM, Path: attest.PathEVM, XPub: pub.String()}
	res, err := f.svc.Attest(context.Background(), "key-admin", q, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := q.Digest()
	x, y := elliptic.Unmarshal(elliptic.P256(), tok.AttestPublicKey())
	ok := ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, d[:], new(big.Int).SetBytes(res.Signature[:32]), new(big.Int).SetBytes(res.Signature[32:]))
	if !ok || res.Statement != string(q.Statement()) {
		t.Fatal("the real token's attestation does not verify over SHA-256(statement)")
	}
	var last audit.Entry
	for _, e := range entries(t, f.logPath) {
		last = e
	}
	if last.Op != audit.OpAttest || last.Result != audit.ResultAuthorized || last.CallerCN != "key-admin" || last.Digest != "0x"+hex.EncodeToString(d[:]) {
		t.Fatalf("audit line %+v", last)
	}
	if _, _, err := audit.Verify(f.logPath, audit.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	// a refused shape never reaches the token and is audited
	bad := &attest.Request{Chain: "solana", Path: attest.PathEVM, XPub: pub.String()}
	if _, err := f.svc.Attest(context.Background(), "key-admin", bad, nil); err == nil {
		t.Fatal("solana attestation accepted")
	}
	t.Log("real SoftHSM token: attestation over the canonical statement verifies (ECDSA P-256/SHA-256), audited before signing; a solana request is refused and audited without reaching the token")
}

func TestSoftHSM_AttestKeyWithWrongAttributesOrCurveIsRefusedAtOpen(t *testing.T) {
	token(t)
	for label, why := range map[string]string{"bad-att-not-private": "CKA_PRIVATE", "bad-att-extractable": "CKA_", "bad-att-secp256k1": "not on P-256"} {
		out, code := runChild(t, "HSMGW_ATTEST_LABEL="+label)
		if code == 0 || !strings.Contains(out, "code="+hsm.CodeKeyAttrs) || !strings.Contains(out, why) {
			t.Errorf("attest label %s: exit %d output %q, want refusal hsm_key_attributes mentioning %s", label, code, out, why)
		}
	}
	// the Hot Key label is not allowed as the attestation label
	if out, code := runChild(t, "HSMGW_ATTEST_LABEL="+keyLabel); code == 0 || !strings.Contains(out, "different keys") {
		t.Errorf("attest label == hot label: %d %q", code, out)
	}
	// a token without an attestation key opens fine, attestation is just unavailable
	if out, code := runChild(t, "HSMGW_ATTEST_LABEL=not-yet-created"); code != 0 || !strings.Contains(out, "attest=false") {
		t.Errorf("pre-M3a token: %d %q", code, out)
	}
}

func TestSoftHSM_InitAttestKeyUpgradesAPreM3aTokenAndRefusesADuplicate(t *testing.T) {
	token(t)
	out, code := runChild(t, "HSMGW_ATTEST_LABEL=late-attest", "HSMGW_ATTEST_INIT=1")
	if code != 0 || !strings.Contains(out, "ok attest_public_key=04") {
		t.Fatalf("init-attest-key: %d %q", code, out)
	}
	if out, code := runChild(t, "HSMGW_ATTEST_LABEL=late-attest"); code != 0 || !strings.Contains(out, "attest=true") {
		t.Fatalf("open after the upgrade: %d %q", code, out)
	}
	if out, code := runChild(t, "HSMGW_ATTEST_LABEL=late-attest", "HSMGW_ATTEST_INIT=1"); code == 0 || !strings.Contains(out, "refusing to create a second attestation key") {
		t.Fatalf("second init-attest-key: %d %q", code, out)
	}
	if out, code := runChild(t, "HSMGW_ATTEST_LABEL="+keyLabel, "HSMGW_ATTEST_INIT=1"); code == 0 {
		t.Fatalf("init-attest-key under the Hot Key label: %d %q", code, out)
	}
}

// ---- M3c: the vault through the service, against the real token

func TestSoftHSM_VaultThroughTheServiceSealsUnsealsRewrapsAndAudits(t *testing.T) {
	tok := token(t)
	f := newFixture(t, tok, 1337)
	open := func(time.Time) (bool, time.Time) { return true, time.Now().Add(time.Minute) }
	f.svc.WithVault(tok, VaultOptions{Window: open, UnsealPerMinute: 1000})
	if !f.svc.VaultEnabled() {
		t.Fatal("the token has no KEK after init-token")
	}
	ctx := context.Background()
	seed := make([]byte, 64)
	for i := range seed {
		seed[i] = byte(i + 9)
	}
	row, err := f.svc.Seal(ctx, "key-admin", &vault.SealRequest{Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: 1, Plaintext: append([]byte(nil), seed...)})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(r *SealedRow) *vault.Row {
		return &vault.Row{Role: r.Role, Purpose: r.Purpose, Version: r.Version, KEKVersion: r.KEKVersion, WrappedDEK: append([]byte(nil), r.WrappedDEK...),
			Nonce: append([]byte(nil), r.Nonce...), Ciphertext: append([]byte(nil), r.Ciphertext...), AAD: append([]byte(nil), r.AAD...)}
	}
	pt, err := f.svc.Unseal(ctx, "sweep-signer", mk(row))
	if err != nil || string(pt) != string(seed) {
		t.Fatalf("unseal: %v", err)
	}
	// every other caller is refused, tampered rows are authentication failures
	if _, err := f.svc.Unseal(ctx, "wallet-service", mk(row)); err == nil {
		t.Fatal("wallet-service unsealed the seed")
	}
	bad := mk(row)
	bad.Ciphertext[2] ^= 1
	if _, err := f.svc.Unseal(ctx, "sweep-signer", bad); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("flipped bit: %v", err)
	}
	// rewrap under a new KEK, then DEK rotation; every row verifies to the same xpub
	if created, err := f.svc.KEKAdmin(ctx, "key-admin", "generate", 2); err != nil && !strings.Contains(err.Error(), "refused") {
		t.Fatalf("generate: %v %v", created, err)
	}
	ks, _ := f.svc.KEKs(ctx)
	hi := ks[len(ks)-1].Version
	if hi < 2 {
		t.Fatalf("no second KEK: %v", ks)
	}
	r2, err := f.svc.Reseal(ctx, "key-admin", &vault.ResealRequest{Row: *mk(row), ToVersion: 2, ToKEK: hi})
	if err != nil || r2.KEKVersion != hi {
		t.Fatalf("rewrap: %v", err)
	}
	r3, err := f.svc.Reseal(ctx, "key-admin", &vault.ResealRequest{Row: *mk(r2), ToVersion: 3, ToKEK: hi, NewDEK: true})
	if err != nil {
		t.Fatalf("rotate-dek: %v", err)
	}
	x1, err1 := f.svc.VerifyXPub(ctx, "key-admin", mk(row))
	x3, err3 := f.svc.VerifyXPub(ctx, "key-admin", mk(r3))
	if err1 != nil || err3 != nil || x1 != x3 || !strings.HasPrefix(x1, "xpub") {
		t.Fatalf("verify: %v %v %q %q", err1, err3, x1, x3)
	}
	// the audit log: one line per operation, the chain verifies
	var ops []string
	for _, e := range entries(t, f.logPath) {
		if e.Class == "vault" {
			ops = append(ops, e.Op)
		}
	}
	t.Logf("real token, through the service: seal, unseal, tamper refusal, KEK generate, rewrap, rotate-dek, verify; audited ops: %s", strings.Join(ops, ","))
	if _, _, err := audit.Verify(f.logPath, audit.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
}
