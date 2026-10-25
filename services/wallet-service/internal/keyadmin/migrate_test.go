package keyadmin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
	"digiwal/services/wallet-service/internal/xpub"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

// fakeGateway stands in for hsm-gateway (the real one is covered by its own tests and by the e2e stack).
type fakeGateway struct {
	key     *ecdsa.PrivateKey // what Attest signs with
	adverts *ecdsa.PrivateKey // the key AttestKey claims (defaults to key)
	attests int
	fail    error
}

func newGW() *fakeGateway {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &fakeGateway{key: k}
}

func (f *fakeGateway) AttestKey(context.Context) ([]byte, error) {
	k := f.adverts
	if k == nil {
		k = f.key
	}
	return elliptic.Marshal(elliptic.P256(), k.X, k.Y), nil //nolint:staticcheck
}

func (f *fakeGateway) Attest(_ context.Context, chain, path, xp string) ([]byte, []byte, error) {
	f.attests++
	if f.fail != nil {
		return nil, nil, f.fail
	}
	d := xpub.Digest(chain, path, xp)
	r, s, _ := ecdsa.Sign(rand.Reader, f.key, d[:])
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return sig, elliptic.Marshal(elliptic.P256(), f.key.X, f.key.Y), nil //nolint:staticcheck
}

type legacy struct {
	s    *storage.Store
	dsn  string
	mn   string
	w    *hdwallet.Wallet
	xp   string
	rows int
}

func newLegacy(t *testing.T, rows int) *legacy {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.New(t)
	s, err := storage.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	mn, _ := hdwallet.NewMnemonic()
	w, _ := hdwallet.FromMnemonic(mn)
	if err := s.InitSeed(ctx, storage.RoleUserDerivation, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := s.CreateAddress(ctx, fmt.Sprintf("u%d", i), "evm", w.AddressAt); err != nil {
			t.Fatal(err)
		}
	}
	xp, _ := hdwallet.AccountXPub(mn)
	return &legacy{s: s, dsn: dsn, mn: mn, w: w, xp: xp, rows: rows}
}

func (l *legacy) noRow(t *testing.T) {
	t.Helper()
	if _, err := l.s.XPub(context.Background(), "evm"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a wallet_xpubs row exists: %v", err)
	}
}

func TestMigrateXPub_DryRunWritesNothing_RealRunStoresAnAttestedRow_RerunIsANoOp(t *testing.T) {
	ctx := context.Background()
	l := newLegacy(t, 40)
	gw := newGW()
	var out bytes.Buffer

	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, l.xp, true, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if gw.attests != 0 {
		t.Fatal("the dry run asked the gateway to attest")
	}
	l.noRow(t)
	if !strings.Contains(out.String(), "reproduced 40/40") || !strings.Contains(out.String(), "DRY RUN") {
		t.Fatalf("dry-run output: %s", out.String())
	}

	out.Reset()
	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, l.xp, false, &out); err != nil {
		t.Fatal(err)
	}
	row, err := l.s.XPub(ctx, "evm")
	if err != nil || row.XPub != l.xp || row.NextIndex != 40 || row.Depth != 3 {
		t.Fatalf("row: %+v %v", row, err)
	}
	if _, err := xpub.Verify(&gw.key.PublicKey, row.Record()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), l.xp) {
		t.Fatal("the output prints the xpub")
	}

	out.Reset()
	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, l.xp, false, &out); err != nil || !strings.Contains(out.String(), "already migrated") {
		t.Fatalf("re-run: %v %s", err, out.String())
	}
	if gw.attests != 1 {
		t.Fatalf("the re-run attested again (%d)", gw.attests)
	}
}

func TestMigrateXPub_RefusesWhenTheXPubDoesNotReproduceTheExistingAddresses(t *testing.T) {
	ctx := context.Background()
	l := newLegacy(t, 25)
	otherMn, _ := hdwallet.NewMnemonic()
	otherXP, _ := hdwallet.AccountXPub(otherMn) // e.g. the sealed seed is not the one that issued these rows
	gw := newGW()
	err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, otherXP, false, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "does not reproduce") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), otherXP) {
		t.Fatal("the error prints the xpub")
	}
	if gw.attests != 0 {
		t.Fatal("the gateway was asked to attest an xpub that does not reproduce the addresses")
	}
	l.noRow(t)

	// one single wrong row among many is enough
	l2 := newLegacy(t, 25)
	conn, _ := pgx.Connect(ctx, l2.dsn)
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE wallet_addresses SET address = '0x0000000000000000000000000000000000000001' WHERE derivation_index = 13`); err != nil {
		t.Fatal(err)
	}
	gw2 := newGW()
	if err := MigrateXPub(ctx, l2.s, gw2, &gw2.key.PublicKey, l2.xp, false, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "index 13") {
		t.Fatalf("one tampered row: %v", err)
	}
	l2.noRow(t)
}

func TestMigrateXPub_RefusesAWrongOrMismatchedAttestationKeyAndABehindCounter(t *testing.T) {
	ctx := context.Background()
	l := newLegacy(t, 5)

	// the gateway's key is not the pinned one
	gw := newGW()
	pinned := newGW()
	if err := MigrateXPub(ctx, l.s, gw, &pinned.key.PublicKey, l.xp, false, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "not the one pinned") {
		t.Fatalf("wrong pin: %v", err)
	}
	// the gateway advertises the pinned key but signs with another one
	liar := newGW()
	liar.adverts = pinned.key
	if err := MigrateXPub(ctx, l.s, liar, &pinned.key.PublicKey, l.xp, false, new(bytes.Buffer)); err == nil {
		t.Fatal("an attestation signed by another key was stored")
	}
	// the gateway refuses
	bad := newGW()
	bad.fail = errors.New("hsm-gateway POST /v1/attest: 503")
	if err := MigrateXPub(ctx, l.s, bad, &bad.key.PublicKey, l.xp, false, new(bytes.Buffer)); err == nil {
		t.Fatal("a failing gateway was ignored")
	}
	l.noRow(t)

	// a counter behind the rows in use
	conn, _ := pgx.Connect(ctx, l.dsn)
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, `UPDATE wallet_seeds SET next_index = 2 WHERE seed_role = 'user_derivation'`)
	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, l.xp, false, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "counter is behind") {
		t.Fatalf("behind counter: %v", err)
	}
	l.noRow(t)
}

func TestMigrateXPub_NeverReplacesADifferentStoredXPub(t *testing.T) {
	ctx := context.Background()
	l := newLegacy(t, 3)
	gw := newGW()
	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, l.xp, false, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	// a second seed's rows: can't happen in one DB, but a different xpub must be refused even when it reproduces nothing to check
	other, _ := hdwallet.NewMnemonic()
	oxp, _ := hdwallet.AccountXPub(other)
	conn, _ := pgx.Connect(ctx, l.dsn)
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, `DELETE FROM wallet_addresses`)
	if err := MigrateXPub(ctx, l.s, gw, &gw.key.PublicKey, oxp, false, new(bytes.Buffer)); err == nil || !strings.Contains(err.Error(), "already holds a different xpub") {
		t.Fatalf("different xpub: %v", err)
	}
	if row, _ := l.s.XPub(ctx, "evm"); row.XPub != l.xp {
		t.Fatal("the stored xpub was replaced")
	}
}
