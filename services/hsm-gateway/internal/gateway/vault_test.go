package gateway

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/prometheus/client_golang/prometheus"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/vault"
)

type vfix struct {
	*fixture
	fv    *fakeVault
	now   time.Time
	mu    sync.Mutex
	until time.Time // zero: the Maintenance Window is closed
}

func (v *vfix) tick(d time.Duration) { v.mu.Lock(); v.now = v.now.Add(d); v.mu.Unlock() }

func newVaultFixture(t *testing.T, specs map[string]vault.Spec, perMin int) *vfix {
	t.Helper()
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	v := &vfix{fixture: f, fv: newFakeVault(), now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	f.svc.WithVault(v.fv, VaultOptions{
		Specs: specs, UnsealPerMinute: perMin, Registry: prometheus.NewRegistry(),
		Now: func() time.Time { v.mu.Lock(); defer v.mu.Unlock(); return v.now },
		Window: func(now time.Time) (bool, time.Time) {
			v.mu.Lock()
			defer v.mu.Unlock()
			return !v.until.IsZero() && now.Before(v.until), v.until
		},
	})
	return v
}

func (v *vfix) openWindow() { v.mu.Lock(); v.until = v.now.Add(15 * time.Minute); v.mu.Unlock() }

func seedBytes() []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

func rowOf(s *SealedRow) *vault.Row {
	return &vault.Row{Role: s.Role, Purpose: s.Purpose, Version: s.Version, KEKVersion: s.KEKVersion,
		WrappedDEK: append([]byte(nil), s.WrappedDEK...), Nonce: append([]byte(nil), s.Nonce...),
		Ciphertext: append([]byte(nil), s.Ciphertext...), AAD: append([]byte(nil), s.AAD...)}
}

func sealSeed(t *testing.T, v *vfix, version int) *SealedRow {
	t.Helper()
	r, err := v.svc.Seal(context.Background(), "key-admin", &vault.SealRequest{Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: version, Plaintext: seedBytes()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVault_RoundTripIsAuditedAndTheAuditLineIsDurableBeforeTheTokenIsAsked(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	if bytes.Contains(row.Ciphertext, seedBytes()[:16]) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	if want := vault.AAD(row.Role, row.Purpose, row.Version, row.KEKVersion, row.WrappedDEK); !bytes.Equal(row.AAD, want) {
		t.Fatalf("aad %q, want %q", row.AAD, want)
	}
	if !strings.HasPrefix(string(row.AAD), "user_derivation|sweep|1|1|") || len(row.Nonce) != 12 {
		t.Fatalf("aad %q nonce %d", row.AAD, len(row.Nonce))
	}

	// the audit line must exist when the token is asked
	var sawAuthorized bool
	v.fv.onUnseal = func() {
		for _, e := range entries(t, v.logPath) {
			if e.Op == audit.OpVaultUnseal && e.Result == audit.ResultAuthorized && e.CallerCN == "sweep-signer" {
				sawAuthorized = true
			}
		}
	}
	pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row))
	if err != nil || !bytes.Equal(pt, seedBytes()) {
		t.Fatalf("unseal: %v", err)
	}
	if !sawAuthorized {
		t.Fatal("the token was asked before the audit line was durable")
	}
	var ops []string
	for _, e := range entries(t, v.logPath) {
		if e.Class == "vault" {
			ops = append(ops, e.Op+"/"+e.Result)
			if strings.Contains(e.Detail, string(seedBytes()[:8])) {
				t.Fatalf("plaintext in the audit line: %+v", e)
			}
		}
	}
	if strings.Join(ops, ",") != "seal/authorized,unseal/authorized" {
		t.Fatalf("audit ops %v", ops)
	}
	if _, _, err := audit.Verify(v.logPath, audit.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestVault_AADTamperSwappedRowsAreRefusedExplicitly(t *testing.T) {
	// a second role, so a row can be swapped between roles
	specs := vault.DefaultSpecs()
	specs["other_role"] = vault.Spec{Purpose: "other", PlainLen: 64, Unseal: []string{"sweep-signer"}}
	v := newVaultFixture(t, specs, 1000)
	a := sealSeed(t, v, 1)
	b, err := v.svc.Seal(context.Background(), "key-admin", &vault.SealRequest{Role: "other_role", Purpose: "other", Version: 1, Plaintext: seedBytes()})
	if err != nil {
		t.Fatal(err)
	}
	a2 := sealSeed(t, v, 2) // another version of the same role

	cases := map[string]func(r *vault.Row){
		"role swapped":             func(r *vault.Row) { r.Role, r.Purpose = "other_role", "other" },
		"purpose swapped":          func(r *vault.Row) { r.Purpose = "other" },
		"version swapped":          func(r *vault.Row) { r.Version = 2 },
		"kek version swapped":      func(r *vault.Row) { r.KEKVersion = 2 },
		"wrapped dek of other row": func(r *vault.Row) { r.WrappedDEK = append([]byte(nil), a2.WrappedDEK...) },
		"aad of other row":         func(r *vault.Row) { r.AAD = append([]byte(nil), a2.AAD...) },
		"flipped ciphertext bit":   func(r *vault.Row) { r.Ciphertext[3] ^= 1 },
		"flipped tag bit":          func(r *vault.Row) { r.Ciphertext[len(r.Ciphertext)-1] ^= 0x80 },
		"flipped wrapped dek bit":  func(r *vault.Row) { r.WrappedDEK[20] ^= 1 },
		"aad rebuilt for a lie":    func(r *vault.Row) { r.Version = 9; r.AAD = vault.AAD(r.Role, r.Purpose, 9, r.KEKVersion, r.WrappedDEK) },
	}
	for name, mut := range cases {
		r := rowOf(a)
		mut(r)
		pt, err := v.svc.Unseal(context.Background(), "sweep-signer", r)
		var vr *VaultRefused
		if pt != nil || err == nil || !(errors.Is(err, ErrAuthFailed) || errors.As(err, &vr)) {
			t.Errorf("%s: got plaintext=%v err=%v, want a refusal", name, pt != nil, err)
		}
	}
	// the explicit class for the tamper cases that reach the token
	r := rowOf(a)
	r.Ciphertext[0] ^= 1
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", r); !errors.Is(err, ErrAuthFailed) {
		t.Errorf("flipped bit: %v, want authentication failed", err)
	}
	// row b is not openable as role user_derivation
	rb := rowOf(b)
	rb.Role, rb.Purpose = vault.RoleUserDerivation, vault.PurposeSweep
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rb); !errors.Is(err, ErrAuthFailed) {
		t.Errorf("row of another role presented as user_derivation: %v", err)
	}
	// and the originals still open
	if pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(a)); err != nil || !bytes.Equal(pt, seedBytes()) {
		t.Fatalf("the untouched row stopped opening: %v", err)
	}
	if pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(a2)); err != nil || !bytes.Equal(pt, seedBytes()) {
		t.Fatalf("version 2 row: %v", err)
	}
	// every refusal is in the audit log
	n := 0
	for _, e := range entries(t, v.logPath) {
		if e.Op == audit.OpVaultUnseal && e.Result != audit.ResultAuthorized {
			n++
		}
	}
	if n < len(cases) {
		t.Errorf("only %d refusals audited for %d tamper cases", n, len(cases))
	}
}

func TestVault_ZeroOrShortNonceIsRefusedBeforeTheToken(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	before := v.fv.unseals.Load()
	for name, n := range map[string][]byte{"zero 12": make([]byte, 12), "empty": nil, "short": row.Nonce[:8], "long": append(append([]byte(nil), row.Nonce...), 1)} {
		r := rowOf(row)
		r.Nonce = n
		if _, err := v.svc.Unseal(context.Background(), "sweep-signer", r); !errors.Is(err, hsm.ErrBadNonce) {
			t.Errorf("%s nonce: %v, want ErrBadNonce", name, err)
		}
	}
	if v.fv.unseals.Load() != before {
		t.Fatal("a bad nonce reached the token")
	}
}

func TestVault_OnlyTheAllowedCallerMayUnsealAndOnlyForItsRole(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	for _, cn := range []string{"wallet-service", "key-admin", "auditor", "sweeper", ""} {
		_, err := v.svc.Unseal(context.Background(), cn, rowOf(row))
		var vr *VaultRefused
		if !errors.As(err, &vr) || vr.Rule != "caller_not_allowed" {
			t.Errorf("%q: %v, want caller_not_allowed", cn, err)
		}
	}
	if v.fv.unseals.Load() != 0 {
		t.Fatal("a refused caller reached the token")
	}
	r := rowOf(row)
	r.Role = "no_such_role"
	var vr *VaultRefused
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", r); !errors.As(err, &vr) || vr.Rule != "role_unknown" {
		t.Errorf("unknown role: %v", err)
	}
	r = rowOf(row)
	r.Purpose = "withdraw"
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", r); !errors.As(err, &vr) || vr.Rule != "purpose_mismatch" {
		t.Errorf("wrong purpose: %v", err)
	}
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); err != nil {
		t.Fatal(err)
	}
}

func TestVault_UnsealIsRateLimitedAndTheLimitIsAudited(t *testing.T) {
	v := newVaultFixture(t, nil, 3)
	row := sealSeed(t, v, 1)
	for i := 0; i < 3; i++ {
		if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); err != nil {
			t.Fatalf("unseal %d: %v", i, err)
		}
	}
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("4th unseal in a minute: %v, want ErrRateLimited", err)
	}
	v.tick(61 * time.Second)
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	found := false
	for _, e := range entries(t, v.logPath) {
		if e.Op == audit.OpVaultUnseal && e.Rule == "rate_limited" && e.Result == audit.ResultRefused {
			found = true
		}
	}
	if !found {
		t.Fatal("the rate-limit refusal is not in the audit log")
	}
}

func TestVault_NoPlaintextWhenTheAuditLogCannotBeWritten(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	_ = v.log.Close()
	pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row))
	if pt != nil || !errors.Is(err, ErrAudit) {
		t.Fatalf("got %v %v, want ErrAudit", pt != nil, err)
	}
	if v.fv.unseals.Load() != 0 {
		t.Fatal("the token opened a row although the audit line could not be written")
	}
	if _, err := v.svc.Seal(context.Background(), "key-admin", &vault.SealRequest{Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: 2, Plaintext: seedBytes()}); !errors.Is(err, ErrAudit) {
		t.Fatalf("seal with a dead log: %v", err)
	}
}

func TestVault_ResealNeedsAMaintenanceWindowAndKeepsThePlaintextAwayFromTheCaller(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	req := &vault.ResealRequest{Row: *rowOf(row), ToVersion: 2}
	if _, err := v.svc.KEKAdmin(context.Background(), "key-admin", "generate", 2); !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("generate-kek without a window: %v", err)
	}
	if _, err := v.svc.Reseal(context.Background(), "key-admin", req); !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("reseal without a window: %v", err)
	}
	v.openWindow()
	if created, err := v.svc.KEKAdmin(context.Background(), "key-admin", "generate", 2); err != nil || !created {
		t.Fatalf("generate: %v %v", created, err)
	}
	if created, err := v.svc.KEKAdmin(context.Background(), "key-admin", "generate", 2); err != nil || created {
		t.Fatalf("generate again must be idempotent: %v %v", created, err)
	}
	if _, err := v.svc.KEKAdmin(context.Background(), "key-admin", "generate", 4); err == nil {
		t.Fatal("a gap in KEK versions was accepted")
	}
	nr, err := v.svc.Reseal(context.Background(), "key-admin", req)
	if err != nil {
		t.Fatal(err)
	}
	if nr.KEKVersion != 2 || nr.Version != 2 || bytes.Equal(nr.WrappedDEK, row.WrappedDEK) || bytes.Equal(nr.Nonce, row.Nonce) {
		t.Fatalf("reseal result %+v", nr)
	}
	// both versions are valid during the rotation; rollback = keep using the old row while kek-v1 exists
	for name, r := range map[string]*SealedRow{"old": row, "new": nr} {
		if pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(r)); err != nil || !bytes.Equal(pt, seedBytes()) {
			t.Fatalf("%s row: %v", name, err)
		}
	}
	// a DEK rotation changes the wrapped DEK under the same KEK
	req2 := &vault.ResealRequest{Row: *rowOf(nr), ToVersion: 3, NewDEK: true}
	nr2, err := v.svc.Reseal(context.Background(), "key-admin", req2)
	if err != nil || nr2.KEKVersion != 2 || bytes.Equal(nr2.WrappedDEK, nr.WrappedDEK) {
		t.Fatalf("rotate-dek: %v %+v", err, nr2)
	}
	if pt, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(nr2)); err != nil || !bytes.Equal(pt, seedBytes()) {
		t.Fatal(err)
	}
	// a row cannot be resealed onto its own version, and a tampered row is not resealed
	if _, err := v.svc.Reseal(context.Background(), "key-admin", &vault.ResealRequest{Row: *rowOf(row), ToVersion: 1}); err == nil {
		t.Fatal("reseal onto the same version accepted")
	}
	bad := *rowOf(row)
	bad.Ciphertext[0] ^= 1
	if _, err := v.svc.Reseal(context.Background(), "key-admin", &vault.ResealRequest{Row: bad, ToVersion: 5}); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("reseal of a tampered row: %v", err)
	}
	// retire v1: the old row still opens (unwrap-only) until v1 is destroyed
	if _, err := v.svc.KEKAdmin(context.Background(), "key-admin", "retire", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); err != nil {
		t.Fatalf("retired KEK must still unwrap: %v", err)
	}
	if _, err := v.svc.KEKAdmin(context.Background(), "key-admin", "destroy", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := v.svc.Unseal(context.Background(), "sweep-signer", rowOf(row)); !errors.Is(err, hsm.ErrNoKEK) {
		t.Fatalf("row under a destroyed KEK: %v, want ErrNoKEK", err)
	}
	// the window closes
	v.tick(16 * time.Minute)
	if _, err := v.svc.KEKAdmin(context.Background(), "key-admin", "retire", 2); !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("after the window: %v", err)
	}
	for _, op := range []string{audit.OpKEKGenerate, audit.OpKEKRetire, audit.OpKEKDestroy, audit.OpVaultReseal} {
		ok := false
		for _, e := range entries(t, v.logPath) {
			if e.Op == op && e.Result == audit.ResultAuthorized && strings.Contains(e.Detail, "window_until=") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("no authorized %s audit line carrying the window", op)
		}
	}
}

func TestVault_VerifyReturnsOnlyThePublicAccountNode(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	row := sealSeed(t, v, 1)
	got, err := v.svc.VerifyXPub(context.Background(), "key-admin", rowOf(row))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := hdkeychain.NewMaster(seedBytes(), &chaincfg.MainNetParams)
	for _, i := range []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart} {
		m, _ = m.Derive(i)
	}
	pub, _ := m.Neuter()
	if got != pub.String() || !strings.HasPrefix(got, "xpub") {
		t.Fatalf("verify returned %q, want %q", got, pub.String())
	}
	bad := rowOf(row)
	bad.Ciphertext[1] ^= 1
	if _, err := v.svc.VerifyXPub(context.Background(), "key-admin", bad); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("verify of a tampered row: %v", err)
	}
}

func TestVault_SealRefusesWrongSizesRolesAndPurposes(t *testing.T) {
	v := newVaultFixture(t, nil, 0)
	for name, q := range map[string]*vault.SealRequest{
		"short seed":    {Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: 1, Plaintext: make([]byte, 32)},
		"unknown role":  {Role: "hot_wallet", Purpose: vault.PurposeSweep, Version: 1, Plaintext: seedBytes()},
		"wrong purpose": {Role: vault.RoleUserDerivation, Purpose: "withdraw", Version: 1, Plaintext: seedBytes()},
		"version 0":     {Role: vault.RoleUserDerivation, Purpose: vault.PurposeSweep, Version: 0, Plaintext: seedBytes()},
	} {
		var vr *VaultRefused
		if _, err := v.svc.Seal(context.Background(), "key-admin", q); !errors.As(err, &vr) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if v.fv.seals.Load() != 0 {
		t.Fatal("a refused seal reached the token")
	}
}

func TestVault_UnavailableOnATokenWithoutAKEK(t *testing.T) {
	be := newSoft(t)
	f := newFixture(t, be, 1337)
	empty := &fakeVault{keks: map[int]*fakeKEK{}}
	f.svc.WithVault(empty, VaultOptions{})
	if f.svc.VaultEnabled() {
		t.Fatal("vault enabled without a KEK")
	}
	if _, err := f.svc.Unseal(context.Background(), "sweep-signer", &vault.Row{}); !errors.Is(err, ErrVaultUnavailable) {
		t.Fatal(err)
	}
}
