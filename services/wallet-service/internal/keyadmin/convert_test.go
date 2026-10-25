//go:build !prod

package keyadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
	"digiwal/services/wallet-service/internal/xpub"
)

const testKey = "0000000000000000000000000000000000000000000000000000000000000001"

// An environment as it exists today: a sealed MNEMONIC under MASTER_KEY_HEX, and addresses issued from it.
type env struct {
	s      *storage.Store
	sealer *crypto.Sealer
	mn     string
	w      *hdwallet.Wallet
	blob   []byte
}

func newEnv(t *testing.T, rows int) *env {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	sealer, _ := crypto.NewSealer(testKey)
	mn, _ := hdwallet.NewMnemonic()
	w, _ := hdwallet.FromMnemonic(mn)
	blob, _ := sealer.Seal([]byte(mn))
	if err := s.InitSeed(ctx, storage.RoleUserDerivation, blob); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := s.CreateAddress(ctx, fmt.Sprintf("u%d", i), "evm", w.AddressAt); err != nil {
			t.Fatal(err)
		}
	}
	return &env{s: s, sealer: sealer, mn: mn, w: w, blob: blob}
}

func (e *env) noSeed64(t *testing.T) {
	t.Helper()
	if _, err := e.s.SealedSeed64(context.Background(), storage.RoleUserDerivation); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("sealed_seed64 exists: %v", err)
	}
}

func TestConvertSeed_DryRunWritesNothing_RealRunStoresTheSealedSeed_RerunVerifies(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 60)
	var out bytes.Buffer

	if err := ConvertSeed(ctx, e.s, e.sealer, true, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	e.noSeed64(t)
	for _, want := range []string{"reproduced 60/60", "indexes 0-99", "DRY RUN"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := ConvertSeed(ctx, e.s, e.sealer, false, &out); err != nil {
		t.Fatal(err)
	}
	blob, err := e.s.SealedSeed64(ctx, storage.RoleUserDerivation)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := e.sealer.OpenAAD(blob, []byte(crypto.SeedAAD))
	if err != nil || len(seed) != 64 {
		t.Fatalf("stored blob does not open as a 64-byte Seed: %v (%d)", err, len(seed))
	}
	want, _ := hdwallet.SeedFromMnemonic(e.mn)
	if !bytes.Equal(seed, want) {
		t.Fatal("the stored Seed is not the mnemonic's BIP39 seed")
	}
	if _, err := e.sealer.Open(blob); err == nil {
		t.Fatal("the Seed blob opens with no AAD")
	}
	// the rollback blob is untouched
	old, _ := e.s.SealedSeed(ctx, storage.RoleUserDerivation)
	if !bytes.Equal(old, e.blob) {
		t.Fatal("the mnemonic-format blob was modified")
	}
	if strings.Contains(out.String(), e.mn) {
		t.Fatal("the output contains the mnemonic")
	}

	out.Reset()
	if err := ConvertSeed(ctx, e.s, e.sealer, false, &out); err != nil || !strings.Contains(out.String(), "already converted") {
		t.Fatalf("re-run: %v %s", err, out.String())
	}
	again, _ := e.s.SealedSeed64(ctx, storage.RoleUserDerivation)
	if !bytes.Equal(again, blob) {
		t.Fatal("the re-run rewrote sealed_seed64")
	}
}

// "Prove all existing addresses reproduce, refuse otherwise": a row that the Seed does not derive stops the conversion and writes nothing.
func TestConvertSeed_RefusesWhenAnExistingAddressIsNotReproduced(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5)
	other, _ := hdwallet.NewMnemonic()
	ow, _ := hdwallet.FromMnemonic(other)
	// a sixth user whose address came from a DIFFERENT seed (a swapped xpub, a restored wrong backup)
	if _, err := e.s.CreateAddress(ctx, "mallory-victim", "evm", ow.AddressAt); err != nil {
		t.Fatal(err)
	}
	err := ConvertSeed(ctx, e.s, e.sealer, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "REFUSED") || !strings.Contains(err.Error(), "does not reproduce") {
		t.Fatalf("got %v", err)
	}
	e.noSeed64(t)
}

func TestConvertSeed_RefusesWhenTheSeedIsNotTheAttestedXPubsSeed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 3)
	gw := newGW()
	other, _ := hdwallet.NewMnemonic()
	otherXP, _ := hdwallet.AccountXPub(other)
	// a wallet_xpubs row of another seed, attested correctly (what a swapped-xpub attack that beat the pin would leave)
	sig, _, _ := gw.Attest(ctx, xpub.ChainEVM, xpub.PathEVM, otherXP)
	if err := e.s.InsertXPub(ctx, storage.XPubRow{Chain: "evm", Path: xpub.PathEVM, Depth: 3, XPub: otherXP, Attestation: sig, NextIndex: 3}); err != nil {
		t.Fatal(err)
	}
	err := ConvertSeed(ctx, e.s, e.sealer, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not the xpub stored in wallet_xpubs") {
		t.Fatalf("got %v", err)
	}
	e.noSeed64(t)
}

func TestConvertSeed_RefusesAWrongMasterKey(t *testing.T) {
	e := newEnv(t, 2)
	wrong, _ := crypto.NewSealer(strings.Repeat("11", 32))
	if err := ConvertSeed(context.Background(), e.s, wrong, false, &bytes.Buffer{}); err == nil {
		t.Fatal("converted with the wrong key")
	}
	e.noSeed64(t)
}

func TestConvertSeed_NothingToConvertInANewEnvironment(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sealer, _ := crypto.NewSealer(testKey)
	if err := ConvertSeed(ctx, s, sealer, false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "nothing to convert") {
		t.Fatalf("got %v", err)
	}
}

// The Seed a new environment gets from init-seed (64-byte format only, no mnemonic blob) is a valid conversion target: the
// seed64-only row opens, its address derivation equals the old library's for the same mnemonic, and a second init is refused.
func TestInitSeed64_NewEnvironment(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sealer, _ := crypto.NewSealer(testKey)
	mn, _ := hdwallet.NewMnemonic()
	seed, _ := hdwallet.SeedFromMnemonic(mn)
	blob, _ := sealer.SealAAD(seed, []byte(crypto.SeedAAD))
	if err := s.InitSeed64(ctx, storage.RoleUserDerivation, blob); err != nil {
		t.Fatal(err)
	}
	if err := s.InitSeed64(ctx, storage.RoleUserDerivation, blob); !errors.Is(err, storage.ErrAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}
	if _, err := s.SealedSeed(ctx, storage.RoleUserDerivation); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a mnemonic-format blob exists for a new-format-only row: %v", err)
	}
	// the xpub migration can start from it
	got, err := s.SealedSeed64(ctx, storage.RoleUserDerivation)
	if err != nil {
		t.Fatal(err)
	}
	opened, _ := sealer.OpenAAD(got, []byte(crypto.SeedAAD))
	a, _ := hdwallet.AddressAtSeed(opened, 7)
	w, _ := hdwallet.FromMnemonic(mn)
	b, _ := w.AddressAt(7)
	if a != b {
		t.Fatalf("index 7: %s vs %s", a, b)
	}
	// the hot_wallet role can never hold a 64-byte-format row (CHECK)
	if err := s.InitSeed64(ctx, storage.RoleHotWallet, blob); err == nil {
		t.Fatal("a hot_wallet seed was stored in the user_derivation Seed format")
	}
}

func TestDevAttester_ProducesAnAttestationTheIssuerAccepts(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 12)
	da, err := NewDevAttester()
	if err != nil {
		t.Fatal(err)
	}
	xp, _ := hdwallet.AccountXPub(e.mn)
	var out bytes.Buffer
	if err := MigrateXPub(ctx, e.s, da, da.Public(), xp, false, &out); err != nil {
		t.Fatal(err)
	}
	row, err := e.s.XPub(ctx, "evm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xpub.Verify(da.Public(), row.Record()); err != nil {
		t.Fatalf("the dev attestation does not verify under its pin: %v", err)
	}
	if got, err := xpub.ParsePinned([]byte(da.PublicHex())); err != nil || xpub.PinnedHex(got) != da.PublicHex() {
		t.Fatalf("PublicHex is not a pin file value: %v", err)
	}
	// another dev attester's pin must not verify it
	other, _ := NewDevAttester()
	if _, err := xpub.Verify(other.Public(), row.Record()); err == nil {
		t.Fatal("a different key's pin verified the attestation")
	}
}
