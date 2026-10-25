package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/gateway"
	"digiwal/services/hsm-gateway/internal/vault"
)

type venv struct {
	*env
	fv *fakeVault
	mu sync.Mutex
	mw time.Time
}

func setupVault(t *testing.T, perMin int) *venv {
	t.Helper()
	v := &venv{fv: newFakeVault()}
	v.env = setupWith(t, true, func(s *gateway.Service) {
		s.WithVault(v.fv, gateway.VaultOptions{UnsealPerMinute: perMin, Registry: prometheus.NewRegistry(),
			Window: func(now time.Time) (bool, time.Time) { v.mu.Lock(); defer v.mu.Unlock(); return now.Before(v.mw), v.mw }})
	})
	return v
}

func (v *venv) openWindow() { v.mu.Lock(); v.mw = time.Now().Add(10 * time.Minute); v.mu.Unlock() }

func (v *venv) raw(c *http.Client, method, path string, body any) (int, []byte, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, v.url+path, rd)
	resp, err := c.Do(req)
	if err != nil {
		return 0, []byte(err.Error()), ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header.Get("Content-Type")
}

func seed64() []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = byte(255 - i)
	}
	return b
}

func (v *venv) seal(t *testing.T, version int) vault.Row {
	t.Helper()
	code, b, _ := v.raw(v.client(v.certs[authz.CNKeyAdmin]), "POST", "/v1/vault/seal",
		vault.SealRequest{Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: version, Plaintext: seed64()})
	if code != 200 {
		t.Fatalf("seal: %d %s", code, b)
	}
	var r vault.Row
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVault_SealThenUnsealOverMTLSReturnsTheRawSeedOnlyToSweepSigner(t *testing.T) {
	v := setupVault(t, 0)
	row := v.seal(t, 1)
	if row.KEKVersion != 1 || len(row.Nonce) != 12 || len(row.WrappedDEK) == 0 || !strings.HasPrefix(string(row.AAD), "user_derivation|sweep|1|1|") {
		t.Fatalf("sealed row %+v", row)
	}
	ss := v.client(v.certs[authz.CNSweepSigner])
	code, b, ct := v.raw(ss, "POST", "/v1/vault/unseal", row)
	if code != 200 || ct != "application/octet-stream" || !bytes.Equal(b, seed64()) {
		t.Fatalf("unseal: %d %q %d bytes", code, ct, len(b))
	}
	// every other CN is refused before the token, with an opaque 403
	for _, cn := range []string{authz.CNWalletService, authz.CNKeyAdmin, authz.CNAuditor} {
		before := v.fv.unseals.Load()
		code, b, _ := v.raw(v.client(v.certs[cn]), "POST", "/v1/vault/unseal", row)
		if code != 403 || bytes.Contains(b, seed64()[:8]) || v.fv.unseals.Load() != before {
			t.Errorf("%s: unseal %d %s", cn, code, b)
		}
	}
	// a CA-signed sweep-signer leaf that is not pinned does not even complete the handshake (K15)
	impostor, _ := v.pki.leaf(t, authz.CNSweepSigner, false)
	if code, b, _ := v.raw(v.client(impostor), "POST", "/v1/vault/unseal", row); code != 0 {
		t.Fatalf("unpinned sweep-signer leaf was served: %d %s", code, b)
	}
	// and sweep-signer cannot seal, verify, reseal or administer KEKs
	for _, p := range []string{"/v1/vault/seal", "/v1/vault/verify", "/v1/vault/reseal", "/v1/vault/kek/generate", "/v1/vault/kek/retire", "/v1/vault/kek/destroy"} {
		if code, _, _ := v.raw(ss, "POST", p, map[string]int{"version": 2}); code != 403 {
			t.Errorf("sweep-signer %s: %d, want 403", p, code)
		}
	}
	if code, _, _ := v.raw(ss, "GET", "/v1/vault/keks", nil); code != 403 {
		t.Errorf("sweep-signer GET keks: %d", code)
	}
	// the audit log has both operations, with the right callers, and nothing secret
	var seen []string
	for _, l := range auditLines(t, v.logP) {
		if l["class"] == "vault" {
			seen = append(seen, l["op"].(string)+":"+l["caller_cn"].(string))
			if l["op"] == "unseal" && !strings.Contains(l["detail"].(string), "wrapped_dek=") {
				t.Errorf("audit detail lacks the wrapped_dek fingerprint: %v", l["detail"])
			}
		}
	}
	if strings.Join(seen, ",") != "seal:key-admin,unseal:sweep-signer" {
		t.Fatalf("audit %v", seen)
	}
}

func TestVault_TamperIsOneExplicitAnswer(t *testing.T) {
	v := setupVault(t, 0)
	row := v.seal(t, 1)
	ss := v.client(v.certs[authz.CNSweepSigner])
	flip := row
	flip.Ciphertext = append([]byte(nil), row.Ciphertext...)
	flip.Ciphertext[0] ^= 1
	code, b, _ := v.raw(ss, "POST", "/v1/vault/unseal", flip)
	if code != 422 || !strings.Contains(string(b), `"authentication_failed"`) {
		t.Fatalf("flipped bit: %d %s", code, b)
	}
	moved := row
	moved.Version = 2
	if code, b, _ := v.raw(ss, "POST", "/v1/vault/unseal", moved); code != 422 || !strings.Contains(string(b), `"authentication_failed"`) {
		t.Fatalf("version moved: %d %s", code, b)
	}
	zeroIV := row
	zeroIV.Nonce = make([]byte, 12)
	if code, b, _ := v.raw(ss, "POST", "/v1/vault/unseal", zeroIV); code != 422 || !strings.Contains(string(b), `"bad_nonce"`) {
		t.Fatalf("zero IV: %d %s", code, b)
	}
	other := row
	other.Role = "hot_wallet"
	if code, b, _ := v.raw(ss, "POST", "/v1/vault/unseal", other); code != 422 || !strings.Contains(string(b), `"role_unknown"`) {
		t.Fatalf("other role: %d %s", code, b)
	}
	// strict decoding: unknown field and trailing data are 400
	for _, body := range []string{`{"role":"user_derivation","purpose":"sweep","version":1,"foo":1}`, `{} {}`, `not json`} {
		req, _ := http.NewRequest("POST", v.url+"/v1/vault/unseal", strings.NewReader(body))
		resp, err := ss.Do(req)
		if err != nil || resp.StatusCode != 400 {
			t.Errorf("body %q: %v %v", body, err, resp)
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}

func TestVault_RateLimitAnd429(t *testing.T) {
	v := setupVault(t, 2)
	row := v.seal(t, 1)
	ss := v.client(v.certs[authz.CNSweepSigner])
	for i := 0; i < 2; i++ {
		if code, _, _ := v.raw(ss, "POST", "/v1/vault/unseal", row); code != 200 {
			t.Fatalf("unseal %d: %d", i, code)
		}
	}
	if code, b, _ := v.raw(ss, "POST", "/v1/vault/unseal", row); code != 429 || !strings.Contains(string(b), "rate_limited") {
		t.Fatalf("3rd unseal: %d %s", code, b)
	}
}

func TestVault_KEKAdminOnlyInsideAMaintenanceWindowAndOnlyKeyAdmin(t *testing.T) {
	v := setupVault(t, 0)
	ka := v.client(v.certs[authz.CNKeyAdmin])
	if code, b, _ := v.raw(ka, "POST", "/v1/vault/kek/generate", vault.KEKRequest{Version: 2}); code != 403 || !strings.Contains(string(b), "maintenance_window_closed") {
		t.Fatalf("generate outside a window: %d %s", code, b)
	}
	v.openWindow()
	if code, b, _ := v.raw(ka, "POST", "/v1/vault/kek/generate", vault.KEKRequest{Version: 2}); code != 200 || !strings.Contains(string(b), `"created":true`) {
		t.Fatalf("generate: %d %s", code, b)
	}
	if code, b, _ := v.raw(ka, "POST", "/v1/vault/kek/generate", vault.KEKRequest{Version: 2}); code != 200 || !strings.Contains(string(b), `"created":false`) {
		t.Fatalf("generate again: %d %s", code, b)
	}
	row := v.seal(t, 1)
	code, b, _ := v.raw(ka, "POST", "/v1/vault/reseal", vault.ResealRequest{Row: row, ToVersion: 2})
	var nr vault.Row
	if code != 200 || json.Unmarshal(b, &nr) != nil || nr.KEKVersion != 2 || nr.Version != 2 {
		t.Fatalf("reseal: %d %s", code, b)
	}
	// the response carries no plaintext field at all
	if bytes.Contains(b, []byte("plaintext")) || bytes.Contains(b, seed64()[:8]) {
		t.Fatal("reseal response carries the plaintext")
	}
	code, b, _ = v.raw(ka, "POST", "/v1/vault/verify", nr)
	if code != 200 || !strings.Contains(string(b), `"xpub":"xpub`) {
		t.Fatalf("verify: %d %s", code, b)
	}
	code, b, _ = v.raw(ka, "GET", "/v1/vault/keks", nil)
	if code != 200 || !strings.Contains(string(b), `"version":2`) {
		t.Fatalf("keks: %d %s", code, b)
	}
	if code, _, _ := v.raw(v.client(v.certs[authz.CNAuditor]), "GET", "/v1/vault/keks", nil); code != 200 {
		t.Fatalf("auditor GET keks: %d", code)
	}
	if code, _, _ := v.raw(v.client(v.certs[authz.CNWalletService]), "GET", "/v1/vault/keks", nil); code != 403 {
		t.Fatalf("wallet-service GET keks: %d", code)
	}
}

func TestVault_503WhenTheTokenHasNoKEK(t *testing.T) {
	e := setup(t)
	code, b, _ := (&venv{env: e}).raw(e.client(e.certs[authz.CNKeyAdmin]), "POST", "/v1/vault/seal",
		vault.SealRequest{Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: 1, Plaintext: seed64()})
	if code != 503 || !strings.Contains(string(b), "vault_unavailable") {
		t.Fatalf("%d %s", code, b)
	}
	_ = audit.OpVaultSeal
	_ = tls.VersionTLS13
}
