package keyadmin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"digiwal/services/wallet-service/internal/seedkit"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/vaultstore"
	"digiwal/services/wallet-service/internal/xpub"
)

// ---- fakes: the real gateway/HSM are covered by hsm-gateway's SoftHSM tests and the e2e stack; the real vault schema by vaultstore_test.go

// memVault models vault.sealed_secrets including its guard trigger (only status moves, only pending rows are deleted, one active).
type memVault struct {
	mu   sync.Mutex
	rows []vaultstore.Row
}

func (m *memVault) List(_ context.Context, role string) ([]vaultstore.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []vaultstore.Row
	for _, r := range m.rows {
		if r.Role == role {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memVault) Insert(_ context.Context, r vaultstore.Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.rows {
		if x.Role == r.Role && x.Version == r.Version {
			return vaultstore.ErrExists
		}
	}
	if r.Status == "" {
		r.Status = vaultstore.Pending
	}
	m.rows = append(m.rows, r)
	return nil
}

func (m *memVault) Activate(_ context.Context, role string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, r := range m.rows {
		if r.Role == role && r.Version == version {
			idx = i
		}
	}
	if idx < 0 {
		return vaultstore.ErrNotFound
	}
	for i := range m.rows {
		if m.rows[i].Role == role && m.rows[i].Status == vaultstore.Active {
			m.rows[i].Status = vaultstore.Retired
		}
	}
	m.rows[idx].Status = vaultstore.Active
	return nil
}

func (m *memVault) DeletePending(_ context.Context, role string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.Role == role && r.Version == version && r.Status == vaultstore.Pending {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return vaultstore.ErrNotFound
}

func (m *memVault) activeRow() (vaultstore.Row, bool) {
	rows, _ := m.List(context.Background(), RoleUserDerivation)
	return active(rows)
}

// memCustody is wallet_xpubs and wallet_addresses.
type memCustody struct {
	mu  sync.Mutex
	row *storage.XPubRow
}

func (c *memCustody) AllAddresses(context.Context, string) ([]storage.AddressRow, error) {
	return nil, nil
}
func (c *memCustody) UserDerivationNextIndex(context.Context) (uint32, error) {
	return 0, errors.New("no wallet_seeds row (a fresh init has none)")
}
func (c *memCustody) XPub(_ context.Context, _ string) (storage.XPubRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.row == nil {
		return storage.XPubRow{}, storage.ErrNotFound
	}
	return *c.row, nil
}
func (c *memCustody) InsertXPub(_ context.Context, r storage.XPubRow) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.row != nil {
		return storage.ErrXPubExists
	}
	c.row = &r
	return nil
}

type fakeKEKState struct {
	key  [32]byte
	wrap bool
}

// swGW is a software model of the gateway's vault half: AES-GCM with the same AAD binding, a wrapped DEK that only opens under its KEK.
type swGW struct {
	mu   sync.Mutex
	keks map[int]*fakeKEKState

	failVerify   atomic.Int32 // number of VerifyXPub calls to fail
	lieAboutXPub atomic.Bool  // VerifyXPub answers another seed's xpub
	reseals      atomic.Int32
}

func newSW() *swGW {
	g := &swGW{keks: map[int]*fakeKEKState{}}
	_, _ = g.GenerateKEK(context.Background(), 1)
	return g
}

func gcmFor(k []byte) cipher.AEAD {
	b, _ := aes.NewCipher(k)
	g, _ := cipher.NewGCM(b)
	return g
}

func aadOf(role, purpose string, version, kek int, wrapped []byte) []byte {
	h := sha256.Sum256(wrapped)
	return []byte(fmt.Sprintf("%s|%s|%d|%d|%x", role, purpose, version, kek, h))
}

func (g *swGW) kek(v int, needWrap bool) (*fakeKEKState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k, ok := g.keks[v]
	if !ok {
		return nil, fmt.Errorf("kek_missing: kek-v%d", v)
	}
	if needWrap && !k.wrap {
		return nil, fmt.Errorf("kek-v%d is retired", v)
	}
	return k, nil
}

func (g *swGW) newest() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	best := 0
	for v, k := range g.keks {
		if k.wrap && v > best {
			best = v
		}
	}
	return best
}

func (g *swGW) encrypt(role, purpose string, version, kv int, dek, pt []byte) Sealed {
	k, _ := g.kek(kv, false)
	wn := make([]byte, 12)
	_, _ = rand.Read(wn)
	wrapped := append(wn, gcmFor(k.key[:]).Seal(nil, wn, dek, nil)...)
	aad := aadOf(role, purpose, version, kv, wrapped)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	return Sealed{Role: role, Purpose: purpose, Version: version, KEKVersion: kv, WrappedDEK: wrapped, Nonce: nonce, Ciphertext: gcmFor(dek).Seal(nil, nonce, pt, aad), AAD: aad}
}

func (g *swGW) open(s Sealed) (dek, pt []byte, err error) {
	if !bytes.Equal(s.AAD, aadOf(s.Role, s.Purpose, s.Version, s.KEKVersion, s.WrappedDEK)) {
		return nil, nil, errors.New("authentication_failed")
	}
	k, err := g.kek(s.KEKVersion, false)
	if err != nil {
		return nil, nil, err
	}
	if dek, err = gcmFor(k.key[:]).Open(nil, s.WrappedDEK[:12], s.WrappedDEK[12:], nil); err != nil {
		return nil, nil, errors.New("authentication_failed")
	}
	if pt, err = gcmFor(dek).Open(nil, s.Nonce, s.Ciphertext, s.AAD); err != nil {
		return nil, nil, errors.New("authentication_failed")
	}
	return dek, pt, nil
}

// Unseal is what sweep-signer asks of the real gateway (not part of key-admin's interface).
func (g *swGW) Unseal(s Sealed) ([]byte, error) { _, pt, err := g.open(s); return pt, err }

func (g *swGW) Seal(_ context.Context, role, purpose string, version, kek int, pt []byte) (Sealed, error) {
	if kek == 0 {
		kek = g.newest()
	}
	if _, err := g.kek(kek, true); err != nil {
		return Sealed{}, err
	}
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	return g.encrypt(role, purpose, version, kek, dek, pt), nil
}

func (g *swGW) VerifyXPub(_ context.Context, s Sealed) (string, error) {
	if g.failVerify.Load() > 0 {
		g.failVerify.Add(-1)
		return "", errors.New("hsm_unavailable (simulated)")
	}
	_, pt, err := g.open(s)
	if err != nil {
		return "", err
	}
	if g.lieAboutXPub.Load() {
		pt = bytes.Repeat([]byte{9}, 64)
	}
	return seedkit.AccountXPubFromSeed(pt)
}

func (g *swGW) Reseal(_ context.Context, s Sealed, toVersion, toKEK int, newDEK bool) (Sealed, error) {
	g.reseals.Add(1)
	dek, pt, err := g.open(s)
	if err != nil {
		return Sealed{}, err
	}
	if toKEK == 0 {
		toKEK = g.newest()
	}
	if _, err := g.kek(toKEK, true); err != nil {
		return Sealed{}, err
	}
	if newDEK {
		dek = make([]byte, 32)
		_, _ = rand.Read(dek)
	}
	return g.encrypt(s.Role, s.Purpose, toVersion, toKEK, dek, pt), nil
}

func (g *swGW) KEKs(context.Context) ([]KEK, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []KEK
	for v := 1; v <= 64; v++ {
		if k, ok := g.keks[v]; ok {
			out = append(out, KEK{Version: v, Wrap: k.wrap})
		}
	}
	return out, nil
}

func (g *swGW) GenerateKEK(_ context.Context, v int) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.keks[v]; ok {
		return false, nil
	}
	if v != len(g.keks)+1 {
		return false, errors.New("versions are consecutive")
	}
	k := &fakeKEKState{wrap: true}
	_, _ = rand.Read(k.key[:])
	g.keks[v] = k
	return true, nil
}

func (g *swGW) RetireKEK(_ context.Context, v int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	k, ok := g.keks[v]
	if !ok {
		return errors.New("kek_missing")
	}
	k.wrap = false
	return nil
}

func (g *swGW) DestroyKEK(_ context.Context, v int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	k, ok := g.keks[v]
	if !ok || k.wrap {
		return errors.New("not retired or missing")
	}
	delete(g.keks, v)
	return nil
}

type vEnv struct {
	d       Deps
	vault   *memVault
	custody *memCustody
	gw      *swGW
	att     *fakeGateway
	out     *bytes.Buffer
	seeds   atomic.Int32
	mn      string
	seed    []byte
}

func newVEnv(t *testing.T) *vEnv {
	t.Helper()
	e := &vEnv{vault: &memVault{}, custody: &memCustody{}, gw: newSW(), att: newGW(), out: &bytes.Buffer{}}
	e.mn, _ = seedkit.NewMnemonic()
	e.seed, _ = seedkit.SeedFromMnemonic(e.mn)
	e.d = Deps{Vault: e.vault, GW: e.gw, Attest: e.att, Custody: e.custody, XStore: e.custody, Pin: &e.att.key.PublicKey, Out: e.out,
		NewSeed: func() (string, []byte, error) {
			e.seeds.Add(1)
			return e.mn, append([]byte(nil), e.seed...), nil
		}}
	return e
}

func (e *vEnv) init(t *testing.T) {
	t.Helper()
	if err := FreshInit(context.Background(), e.d, false); err != nil {
		t.Fatalf("init: %v\n%s", err, e.out)
	}
}

func TestFreshInit_SealsVerifiesAttestsActivatesAndProvesParity(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	rows, _ := e.vault.List(context.Background(), RoleUserDerivation)
	if len(rows) != 1 || rows[0].Status != vaultstore.Active || rows[0].Version != 1 || rows[0].KEKVersion != 1 || rows[0].Purpose != PurposeSweep {
		t.Fatalf("rows: %+v", rows)
	}
	if bytes.Contains(rows[0].Ciphertext, e.seed[:16]) || bytes.Contains(rows[0].WrappedDEK, e.seed[:16]) {
		t.Fatal("the seed is in the stored row")
	}
	if pt, err := e.gw.Unseal(SealedOf(rows[0])); err != nil || !bytes.Equal(pt, e.seed) {
		t.Fatalf("the stored row does not open to the seed: %v", err)
	}
	x, err := e.custody.XPub(context.Background(), "evm")
	want, _ := seedkit.AccountXPubFromSeed(e.seed)
	if err != nil || x.XPub != want || x.NextIndex != 0 {
		t.Fatalf("stored xpub row: %+v %v", x, err)
	}
	if _, err := xpub.Verify(&e.att.key.PublicKey, x.Record()); err != nil {
		t.Fatalf("the stored attestation does not verify against the pin: %v", err)
	}
	o := e.out.String()
	if strings.Count(o, e.mn) != 1 {
		t.Fatalf("the mnemonic is shown %d times, want exactly once", strings.Count(o, e.mn))
	}
	if strings.Count(o, "parity:") != 2 || !strings.Contains(o, "INITIALISED") {
		t.Fatalf("output:\n%s", o)
	}
	// the mnemonic is shown after the row is stored and verified, before the xpub is attested
	if strings.Index(o, e.mn) < strings.Index(o, "verified: hsm-gateway opens") || strings.Index(o, e.mn) > strings.Index(o, "INITIALISED") {
		t.Fatalf("the mnemonic is shown out of order:\n%s", o)
	}
}

func TestFreshInit_RefusesWhenASealedSecretExistsAndGeneratesNoNewSeed(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	n := e.seeds.Load()
	before, _ := e.vault.List(context.Background(), RoleUserDerivation)
	for _, resume := range []bool{false, true} {
		if err := FreshInit(context.Background(), e.d, resume); !errors.Is(err, ErrInitialised) {
			t.Fatalf("second init (resume=%v): %v", resume, err)
		}
	}
	after, _ := e.vault.List(context.Background(), RoleUserDerivation)
	if e.seeds.Load() != n || len(after) != len(before) {
		t.Fatal("a refused init generated a seed or wrote a row")
	}
	// the same for a pending row without -resume, and a retired one
	for _, st := range []string{vaultstore.Pending, vaultstore.Retired} {
		e2 := newVEnv(t)
		_ = e2.vault.Insert(context.Background(), vaultstore.Row{Role: RoleUserDerivation, Version: 1, Status: st, Purpose: PurposeSweep, KEKVersion: 1})
		if err := FreshInit(context.Background(), e2.d, false); !errors.Is(err, ErrInitialised) {
			t.Fatalf("init over a %s row: %v", st, err)
		}
	}
}

func TestFreshInit_InterruptedAfterSealingLeavesAnInertPendingRowAndResumes(t *testing.T) {
	e := newVEnv(t)
	e.att.fail = errors.New("hsm-gateway went away")
	err := FreshInit(context.Background(), e.d, false)
	if err == nil || !strings.Contains(err.Error(), "init -resume") {
		t.Fatalf("interrupted init: %v", err)
	}
	if _, ok := e.vault.activeRow(); ok {
		t.Fatal("an interrupted init left an ACTIVE row: sweep-signer could read a seed whose xpub was never attested")
	}
	rows, _ := e.vault.List(context.Background(), RoleUserDerivation)
	if len(rows) != 1 || rows[0].Status != vaultstore.Pending {
		t.Fatalf("rows %+v", rows)
	}
	if !strings.Contains(e.out.String(), e.mn) {
		t.Fatal("the mnemonic must already have been shown: the pending row exists")
	}
	if err := FreshInit(context.Background(), e.d, false); !errors.Is(err, ErrInitialised) {
		t.Fatalf("init without -resume over a pending row: %v", err)
	}
	e.att.fail = nil
	n := e.seeds.Load()
	if err := FreshInit(context.Background(), e.d, true); err != nil {
		t.Fatalf("resume: %v\n%s", err, e.out)
	}
	if e.seeds.Load() != n {
		t.Fatal("resume generated a new seed")
	}
	if r, ok := e.vault.activeRow(); !ok || r.Version != 1 {
		t.Fatal("resume did not activate")
	}
	want, _ := seedkit.AccountXPubFromSeed(e.seed)
	if x, _ := e.custody.XPub(context.Background(), "evm"); x.XPub != want {
		t.Fatal("resume stored another xpub")
	}
	// idempotent from here
	if err := FreshInit(context.Background(), e.d, true); !errors.Is(err, ErrInitialised) {
		t.Fatalf("resume after completion: %v", err)
	}
}

func TestFreshInit_RefusesWhenTheGatewayOpensAnotherSeed(t *testing.T) {
	e := newVEnv(t)
	e.gw.lieAboutXPub.Store(true)
	err := FreshInit(context.Background(), e.d, false)
	if err == nil || !strings.Contains(err.Error(), "different seed") {
		t.Fatalf("got %v", err)
	}
	if e.att.attests != 0 {
		t.Fatal("attested although the sealed row does not hold the seed")
	}
	if _, ok := e.vault.activeRow(); ok {
		t.Fatal("activated")
	}
	if strings.Contains(e.out.String(), e.mn) {
		t.Fatal("the mnemonic was shown for a row that does not hold it")
	}
}

func TestRotateKEK_GeneratesRewrapsVerifiesFlipsAndIsIdempotent(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
		t.Fatalf("rotate-kek: %v\n%s", err, e.out)
	}
	rows, _ := e.vault.List(ctx, RoleUserDerivation)
	if len(rows) != 2 || rows[0].Status != vaultstore.Retired || rows[1].Status != vaultstore.Active || rows[1].KEKVersion != 2 || rows[1].Version != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if bytes.Equal(rows[0].WrappedDEK, rows[1].WrappedDEK) {
		t.Fatal("the wrapped DEK did not change")
	}
	for _, r := range rows {
		if pt, err := e.gw.Unseal(SealedOf(r)); err != nil || !bytes.Equal(pt, e.seed) {
			t.Fatalf("version %d does not open: %v", r.Version, err)
		}
	}
	// a second run: nothing to do, no new KEK, no new row
	n := e.gw.reseals.Load()
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err == nil {
		// rotate-kek again generates kek-v3 because the active row is already on the newest: that is a NEW rotation, not a repeat
		rows, _ = e.vault.List(ctx, RoleUserDerivation)
		if len(rows) != 3 || rows[2].KEKVersion != 3 {
			t.Fatalf("second rotate-kek: %+v", rows)
		}
	} else {
		t.Fatal(err)
	}
	// rewrap to the version it is already under: nothing to do
	m := e.gw.reseals.Load()
	if err := Rotate(ctx, e.d, RotateOpts{ToKEK: 3}); err != nil || e.gw.reseals.Load() != m {
		t.Fatalf("rewrap onto the same KEK must be a no-op: %v", err)
	}
	_ = n
	if !strings.Contains(e.out.String(), "nothing to do") {
		t.Fatal("no 'nothing to do' message")
	}
}

func TestRewrapLeavesPendingAndActivateFlipsAndRollbackFlipsBack(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	if _, err := e.gw.GenerateKEK(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(ctx, e.d, RotateOpts{ToKEK: 2}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.vault.activeRow(); r.Version != 1 {
		t.Fatal("rewrap flipped")
	}
	rows, _ := e.vault.List(ctx, RoleUserDerivation)
	if len(rows) != 2 || rows[1].Status != vaultstore.Pending {
		t.Fatalf("rows %+v", rows)
	}
	if err := Activate(ctx, e.d); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.vault.activeRow(); r.Version != 2 || r.KEKVersion != 2 {
		t.Fatalf("active %+v", r)
	}
	if err := Rollback(ctx, e.d); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.vault.activeRow(); r.Version != 1 {
		t.Fatalf("after rollback active is version %d", r.Version)
	}
	// rotating again after a rollback picks a version above every existing one
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
		t.Fatalf("rotate after rollback: %v", err)
	}
	if r, _ := e.vault.activeRow(); r.Version != 3 {
		t.Fatalf("version after rollback and rotate: %d", r.Version)
	}
}

func TestRotateDEK_ReencryptsUnderTheSameKEK(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	before, _ := e.vault.activeRow()
	if err := Rotate(ctx, e.d, RotateOpts{NewDEK: true, Flip: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := e.vault.activeRow()
	if after.Version != 2 || after.KEKVersion != before.KEKVersion || bytes.Equal(after.WrappedDEK, before.WrappedDEK) || bytes.Equal(after.Ciphertext, before.Ciphertext) {
		t.Fatalf("rotate-dek: %+v -> %+v", before, after)
	}
	if pt, err := e.gw.Unseal(SealedOf(after)); err != nil || !bytes.Equal(pt, e.seed) {
		t.Fatal(err)
	}
}

func TestRotation_InterruptedIsInertAndARerunFinishes(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	// the verification fails once after the pending row was written (the gateway dies mid-rotation)
	e.gw.failVerify.Store(1)
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err == nil {
		t.Fatal("rotation succeeded although verification failed")
	}
	if r, _ := e.vault.activeRow(); r.Version != 1 {
		t.Fatal("an interrupted rotation changed the active row")
	}
	rows, _ := e.vault.List(ctx, RoleUserDerivation)
	for _, r := range rows {
		if r.Version > 1 && r.Status != vaultstore.Pending {
			t.Fatalf("leftover row %+v", r)
		}
	}
	// a stale pending row written by a crashed run is reused when it verifies and fits, and the rerun completes
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
		t.Fatalf("rerun: %v\n%s", err, e.out)
	}
	if r, _ := e.vault.activeRow(); r.KEKVersion != 2 {
		t.Fatalf("after the rerun the active row is under kek-v%d", r.KEKVersion)
	}
	// crash AFTER inserting a pending row but before verifying (simulated by inserting one by hand), with another target: it is replaced
	e2 := newVEnv(t)
	e2.init(t)
	cur, _ := e2.vault.activeRow()
	stale, _ := e2.gw.Reseal(ctx, SealedOf(cur), 2, 1, true)
	_ = e2.vault.Insert(ctx, RowOf(stale))
	_, _ = e2.gw.GenerateKEK(ctx, 2)
	if err := Rotate(ctx, e2.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
		t.Fatalf("rotate over a stale pending row: %v", err)
	}
	if r, _ := e2.vault.activeRow(); r.KEKVersion != 2 {
		t.Fatalf("active %+v", r)
	}
	if !strings.Contains(e2.out.String(), "stale pending") {
		t.Fatalf("output:\n%s", e2.out)
	}
}

func TestRotation_RefusesAndDeletesAPendingRowThatOpensToAnotherSeed(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	e.gw.lieAboutXPub.Store(true)
	err := Rotate(context.Background(), e.d, RotateOpts{Flip: true, GenerateKEK: true})
	if err == nil || !strings.Contains(err.Error(), "different seed") {
		t.Fatalf("got %v", err)
	}
	rows, _ := e.vault.List(context.Background(), RoleUserDerivation)
	if len(rows) != 1 || rows[0].Status != vaultstore.Active {
		t.Fatalf("rows %+v", rows)
	}
}

func TestRotation_RefusesWhenTheStoredXPubDoesNotVerifyAgainstThePin(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	other := newGW()
	e.d.Pin = &other.key.PublicKey
	if err := Rotate(context.Background(), e.d, RotateOpts{Flip: true, GenerateKEK: true}); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("got %v", err)
	}
	rows, _ := e.vault.List(context.Background(), RoleUserDerivation)
	if len(rows) != 1 {
		t.Fatal("a row was written before the pin check")
	}
}

func TestRetireAndDestroyKEKRefuseToStrandLiveRows(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	if err := RetireKEK(ctx, e.d, 1); err == nil {
		t.Fatal("retired the KEK the active row is wrapped under")
	}
	if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
		t.Fatal(err)
	}
	if err := DestroyKEK(ctx, e.d, 1, false); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("destroy of a KEK a retired row needs: %v", err)
	}
	if err := RetireKEK(ctx, e.d, 1); err != nil {
		t.Fatal(err)
	}
	// rollback still works while kek-v1 exists (unwrap-only), and is impossible once it is destroyed
	if err := Rollback(ctx, e.d); err != nil {
		t.Fatalf("rollback onto a retired (unwrap-only) KEK: %v", err)
	}
	if err := Activate2(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := DestroyKEK(ctx, e.d, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(ctx, e.d); err == nil {
		t.Fatal("rolled back onto a row whose KEK was destroyed")
	}
}

// Activate2 flips back to version 2 (the rotation's row) for the test above.
func Activate2(ctx context.Context, e *vEnv) error {
	return e.vault.Activate(ctx, RoleUserDerivation, 2)
}

// A Sweep that reads the active row per request must never fail while a rotation flips, retires and (later) destroys the old KEK.
func TestRotationWhileSweepsRunConcurrently(t *testing.T) {
	e := newVEnv(t)
	e.init(t)
	ctx := context.Background()
	var stop atomic.Bool
	var ops, errs atomic.Int64
	var first atomic.Value
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				row, ok := e.vault.activeRow() // sweep-signer: the active row, per request
				if !ok {
					errs.Add(1)
					first.CompareAndSwap(nil, "no active row")
					continue
				}
				pt, err := e.gw.Unseal(SealedOf(row))
				ops.Add(1)
				if err != nil || !bytes.Equal(pt, e.seed) {
					errs.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("version %d kek-v%d: %v", row.Version, row.KEKVersion, err))
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if err := Rotate(ctx, e.d, RotateOpts{Flip: true, GenerateKEK: true}); err != nil {
			t.Fatal(err)
		}
		if err := Rotate(ctx, e.d, RotateOpts{Flip: true, NewDEK: true}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := RetireKEK(ctx, e.d, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d of %d concurrent sweeps failed during the rotation, first: %v", errs.Load(), ops.Load(), first.Load())
	}
	t.Logf("%d concurrent unseals of the active row across 3 kek rotations, 3 dek rotations and a retire: 0 errors", ops.Load())
}

func TestStatusReportsInitialisation(t *testing.T) {
	e := newVEnv(t)
	if ok, err := Status(context.Background(), e.d); err != nil || ok {
		t.Fatalf("empty: %v %v", ok, err)
	}
	e.init(t)
	e.out.Reset()
	if ok, err := Status(context.Background(), e.d); err != nil || !ok || !strings.Contains(e.out.String(), "active") || !strings.Contains(e.out.String(), "kek-v1  wrap=true") {
		t.Fatalf("%v %v\n%s", ok, err, e.out)
	}
}
