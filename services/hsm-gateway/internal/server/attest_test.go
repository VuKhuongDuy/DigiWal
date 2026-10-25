package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/hsm"
)

const h = hdkeychain.HardenedKeyStart

// acctXPub is the account node of a throwaway seed; extra deviates from m/44'/60'/0'.
func xpubAt(t *testing.T, path ...uint32) string {
	t.Helper()
	seed := make([]byte, 64)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range path {
		if k, err = k.Derive(i); err != nil {
			t.Fatal(err)
		}
	}
	n, err := k.Neuter()
	if err != nil {
		t.Fatal(err)
	}
	return n.String()
}

func goodXPub(t *testing.T) string { return xpubAt(t, h+44, h+60, h+0) }

func attestBody(xpub string) string {
	return `{"chain":"evm","path":"m/44'/60'/0'","xpub":"` + xpub + `"}`
}

func auditLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestAttest_SignsTheCanonicalStatementAndAuditsIt(t *testing.T) {
	e := setup(t)
	ka := e.client(e.certs[authz.CNKeyAdmin])
	x := goodXPub(t)

	code, m, err := e.do(ka, "POST", "/v1/attest", attestBody(x))
	if err != nil || code != 200 {
		t.Fatalf("attest: %d %v %v", code, m, err)
	}
	want := "digiwal/xpub-attest/v1\nchain=evm\npath=m/44'/60'/0'\nxpub=" + x + "\n"
	if m["statement"] != want || m["alg"] != "ecdsa-p256-sha256" {
		t.Fatalf("statement/alg: %q %v", m["statement"], m["alg"])
	}
	sig, _ := hex.DecodeString(m["signature"].(string))
	pub, _ := hex.DecodeString(m["public_key"].(string))
	if len(sig) != 64 || len(pub) != 65 {
		t.Fatalf("sig %d bytes, pub %d bytes", len(sig), len(pub))
	}
	px, py := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	d := sha256.Sum256([]byte(want))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: px, Y: py}, d[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the signature does not verify over SHA-256(statement) under the returned public key")
	}
	// GET /v1/attest/key is the same key
	code, km, _ := e.do(ka, "GET", "/v1/attest/key", "")
	if code != 200 || km["public_key"] != m["public_key"] || km["alg"] != "ecdsa-p256-sha256" {
		t.Fatalf("GET key: %d %v", code, km)
	}
	// the Hot Key is a different key (its address is not the attestation key, and Hot GET still serves)
	if code, hk, _ := e.do(e.client(e.certs[authz.CNWalletService]), "GET", "/v1/hot/key", ""); code != 200 || strings.Contains(hk["public_key"].(string), m["public_key"].(string)) {
		t.Fatalf("hot key response: %d %v", code, hk)
	}

	// audit: one authorized attest line from key-admin carrying the digest, then the chain still verifies
	lines := auditLines(t, e.logP)
	var found map[string]any
	for _, l := range lines {
		if l["op"] == audit.OpAttest {
			found = l
		}
	}
	if found == nil || found["caller_cn"] != "key-admin" || found["result"] != audit.ResultAuthorized || found["digest"] != "0x"+hex.EncodeToString(d[:]) || found["class"] != "xpub_attest" {
		t.Fatalf("audit line: %v", found)
	}
	if strings.Contains(found["detail"].(string), x) {
		t.Fatalf("the audit line contains the full xpub: %v", found["detail"])
	}
	if seq, _, err := audit.Verify(e.logP, audit.VerifyOptions{}); err != nil || float64(seq) != found["seq"].(float64) {
		t.Fatalf("audit chain: seq %d err %v", seq, err)
	}
}

func TestAttest_CNAllowList(t *testing.T) {
	e := setup(t)
	body := attestBody(goodXPub(t))
	for _, c := range []struct {
		cn, method, path string
		want             int
	}{
		{authz.CNKeyAdmin, "POST", "/v1/attest", 200},
		{authz.CNWalletService, "POST", "/v1/attest", 403},
		{authz.CNSweepSigner, "POST", "/v1/attest", 403},
		{authz.CNAuditor, "POST", "/v1/attest", 403},
		{authz.CNKeyAdmin, "GET", "/v1/attest/key", 200},
		{authz.CNAuditor, "GET", "/v1/attest/key", 200},
		{authz.CNWalletService, "GET", "/v1/attest/key", 403},
		{authz.CNSweepSigner, "GET", "/v1/attest/key", 403},
	} {
		b := ""
		if c.method == "POST" {
			b = body
		}
		before := e.be.attSigns.Load()
		code, _, err := e.do(e.client(e.certs[c.cn]), c.method, c.path, b)
		if err != nil || code != c.want {
			t.Errorf("%s %s as %s: %d (%v), want %d", c.method, c.path, c.cn, code, err, c.want)
		}
		if c.want == 403 && e.be.attSigns.Load() != before {
			t.Errorf("%s reached the token", c.cn)
		}
	}
	// a CA-signed key-admin leaf that is not pinned is refused in the handshake (K15)
	impostor, _ := e.pki.leaf(t, authz.CNKeyAdmin, false)
	if code, _, err := e.do(e.client(impostor), "POST", "/v1/attest", body); err == nil {
		t.Fatalf("unpinned key-admin leaf was served (%d)", code)
	}
}

func TestAttest_RefusesUnknownStatementShapes(t *testing.T) {
	e := setup(t)
	ka := e.client(e.certs[authz.CNKeyAdmin])
	good := goodXPub(t)
	priv := func() string {
		seed := make([]byte, 64)
		k, _ := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
		return k.String()
	}()
	for name, b := range map[string]string{
		"solana chain":      `{"chain":"solana","path":"m/44'/60'/0'","xpub":"` + good + `"}`,
		"other path":        `{"chain":"evm","path":"m/44'/60'/1'","xpub":"` + good + `"}`,
		"account 1 xpub":    attestBody(xpubAt(t, h+44, h+60, h+1)),
		"depth 4 xpub":      attestBody(xpubAt(t, h+44, h+60, h+0, 0)),
		"private key":       attestBody(priv),
		"garbage xpub":      attestBody("not-an-xpub"),
		"caller digest":     `{"chain":"evm","path":"m/44'/60'/0'","xpub":"` + good + `","digest":"0x00"}`,
		"caller statement":  `{"chain":"evm","path":"m/44'/60'/0'","xpub":"` + good + `","statement":"anything"}`,
		"trailing object":   attestBody(good) + attestBody(good),
		"not json":          `{`,
		"newline injection": attestBody(good + `\nchain=solana`),
	} {
		before, nLines := e.be.attSigns.Load(), len(auditLines(t, e.logP))
		code, m, err := e.do(ka, "POST", "/v1/attest", b)
		if err != nil || code != 422 || m["error"] != "attest_refused" || m["signature"] != nil {
			t.Errorf("%s: %d %v %v", name, code, m, err)
		}
		if e.be.attSigns.Load() != before {
			t.Errorf("%s: the token was asked to sign", name)
		}
		lines := auditLines(t, e.logP)
		if len(lines) != nLines+1 || lines[len(lines)-1]["op"] != audit.OpAttest || lines[len(lines)-1]["result"] != audit.ResultRefused || lines[len(lines)-1]["rule"] != "attest_shape" {
			t.Errorf("%s: no refused audit line (%d -> %d lines)", name, nLines, len(lines))
		}
	}
	// and the allowed shape still signs afterwards
	if code, _, _ := e.do(ka, "POST", "/v1/attest", attestBody(good)); code != 200 {
		t.Fatalf("good request after refusals: %d", code)
	}
}

func TestAttest_TokenWithoutKeyAndHSMFailureAreExplicit(t *testing.T) {
	e := setup(t)
	ka := e.client(e.certs[authz.CNKeyAdmin])
	// HSM failure: 503 with the class, an authorized line followed by an hsm_error line, no signature
	e.be.fail = &hsm.Error{Code: hsm.CodeUnavailable, Op: "attest"}
	code, m, _ := e.do(ka, "POST", "/v1/attest", attestBody(goodXPub(t)))
	if code != 503 || m["error"] != hsm.CodeUnavailable || m["signature"] != nil {
		t.Fatalf("hsm failure: %d %v", code, m)
	}
	lines := auditLines(t, e.logP)
	if n := len(lines); n < 2 || lines[n-2]["result"] != audit.ResultAuthorized || lines[n-1]["result"] != audit.ResultHSMError {
		t.Fatalf("audit lines after hsm failure: %v", lines[len(lines)-2:])
	}
	e.be.fail = nil

	// a token initialised before M3a has no attestation key: 503 attest_unavailable, nothing audited as signed
	e2s := setupWithoutAttest(t)
	ka2 := e2s.client(e2s.certs[authz.CNKeyAdmin])
	if code, m, _ := e2s.do(ka2, "POST", "/v1/attest", attestBody(goodXPub(t))); code != 503 || m["error"] != "attest_unavailable" {
		t.Fatalf("no key: %d %v", code, m)
	}
	if code, m, _ := e2s.do(ka2, "GET", "/v1/attest/key", ""); code != 503 || m["error"] != "attest_unavailable" {
		t.Fatalf("no key GET: %d %v", code, m)
	}
}
