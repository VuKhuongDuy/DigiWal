package xpub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"

	"digiwal/services/wallet-service/internal/hdwallet"
)

// Hardhat's public test mnemonic: its first accounts are published, an independent vector for the derivation.
const hardhatMnemonic = "test test test test test test test test test test test junk"

func TestAddressAt_MatchesPublishedHardhatAccounts(t *testing.T) {
	xp, err := hdwallet.AccountXPub(hardhatMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := ParseAccountKey(xp)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"} {
		got, err := AddressAt(acct, uint32(i))
		if err != nil || got != want {
			t.Fatalf("index %d: %s %v, want %s", i, got, err, want)
		}
	}
}

// key-custody-hsm.md invariant 12 / M3a: every derivation index reproduces the address the seed-based wallet gives.
func TestAddressAt_ParityWithTheSeedWalletForIndexes0To1000(t *testing.T) {
	mn, err := hdwallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	w, err := hdwallet.FromMnemonic(mn)
	if err != nil {
		t.Fatal(err)
	}
	xp, err := hdwallet.AccountXPub(mn)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := ParseAccountKey(xp)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i <= 1000; i++ {
		want, err := w.AddressAt(i)
		if err != nil {
			t.Fatal(err)
		}
		got, err := AddressAt(acct, i)
		if err != nil || got != want {
			t.Fatalf("index %d: xpub gives %s (%v), seed gives %s", i, got, err, want)
		}
	}
	t.Logf("1001/1001 indexes (0..1000) of a fresh 24-word mnemonic: xpub public derivation == go-ethereum-hdwallet private derivation")
}

func TestAddressAt_RefusesHardenedIndexAndPrivateKeys(t *testing.T) {
	xp, _ := hdwallet.AccountXPub(hardhatMnemonic)
	acct, _ := ParseAccountKey(xp)
	if _, err := AddressAt(acct, hdkeychain.HardenedKeyStart); err == nil {
		t.Fatal("a hardened index was derived")
	}
	priv, _ := hdkeychain.NewMaster(make([]byte, 32), &chaincfg.MainNetParams)
	if _, err := AddressAt(priv, 0); err == nil {
		t.Fatal("derived from a private extended key")
	}
}

// ---- attestation

type signer struct{ key *ecdsa.PrivateKey }

func newSigner(t *testing.T) *signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{k}
}

func (s *signer) attest(chain, path, xpub string) []byte {
	d := Digest(chain, path, xpub)
	r, sv, _ := ecdsa.Sign(rand.Reader, s.key, d[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sv.FillBytes(out[32:])
	return out
}

func (s *signer) record(t *testing.T, mnemonic string) Record {
	xp, err := hdwallet.AccountXPub(mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return Record{Chain: ChainEVM, Path: PathEVM, XPub: xp, Attestation: s.attest(ChainEVM, PathEVM, xp)}
}

func TestVerify_AcceptsTheAttestedRecordAndRefusesEverySwapAndTamper(t *testing.T) {
	hsm := newSigner(t)
	good := hsm.record(t, hardhatMnemonic)
	if _, err := Verify(&hsm.key.PublicKey, good); err != nil {
		t.Fatalf("the genuine record was refused: %v", err)
	}
	attackerMn, _ := hdwallet.NewMnemonic()
	attacker, _ := hdwallet.AccountXPub(attackerMn)
	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 1; return c }
	otherKey := newSigner(t)

	for name, rec := range map[string]Record{
		"xpub swapped for the attacker's, attestation kept": {Chain: ChainEVM, Path: PathEVM, XPub: attacker, Attestation: good.Attestation},
		"xpub swapped and re-signed by the attacker's key":  {Chain: ChainEVM, Path: PathEVM, XPub: attacker, Attestation: otherKey.attest(ChainEVM, PathEVM, attacker)},
		"attestation bit flipped (r)":                       {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub, Attestation: flip(good.Attestation, 3)},
		"attestation bit flipped (s)":                       {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub, Attestation: flip(good.Attestation, 40)},
		"attestation truncated":                             {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub, Attestation: good.Attestation[:63]},
		"attestation empty":                                 {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub},
		"attestation of all zeros":                          {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub, Attestation: make([]byte, 64)},
		"path edited":                                       {Chain: ChainEVM, Path: "m/44'/60'/1'", XPub: good.XPub, Attestation: good.Attestation},
		"chain edited":                                      {Chain: "solana", Path: PathEVM, XPub: good.XPub, Attestation: good.Attestation},
		"attestation over another chain's statement":        {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub, Attestation: hsm.attest("bitcoin", PathEVM, good.XPub)},
		"xpub garbage":                                      {Chain: ChainEVM, Path: PathEVM, XPub: "xpub-garbage", Attestation: good.Attestation},
		"xpub with a trailing newline":                      {Chain: ChainEVM, Path: PathEVM, XPub: good.XPub + "\n", Attestation: good.Attestation},
	} {
		if _, err := Verify(&hsm.key.PublicKey, rec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// signed by the HSM but a private key, a wrong depth or a wrong account is still refused
	seed := make([]byte, 64)
	m, _ := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	depth1, _ := m.Derive(hdkeychain.HardenedKeyStart + 44)
	depth1Pub, _ := depth1.Neuter()
	acct1, _ := m.Derive(hdkeychain.HardenedKeyStart + 44)
	acct1, _ = acct1.Derive(hdkeychain.HardenedKeyStart + 60)
	acct1, _ = acct1.Derive(hdkeychain.HardenedKeyStart + 1)
	acct1Pub, _ := acct1.Neuter()
	for name, x := range map[string]string{"private key": m.String(), "depth 1": depth1Pub.String(), "account 1": acct1Pub.String()} {
		rec := Record{Chain: ChainEVM, Path: PathEVM, XPub: x, Attestation: hsm.attest(ChainEVM, PathEVM, x)}
		if _, err := Verify(&hsm.key.PublicKey, rec); err == nil {
			t.Errorf("HSM-signed %s was accepted", name)
		}
	}
	// the right record under another pinned key
	if _, err := Verify(&otherKey.key.PublicKey, good); err == nil {
		t.Error("verified under a different pinned key")
	}
	if _, err := Verify(nil, good); err == nil {
		t.Error("verified with no pin")
	}
}

func TestIssuer_FailureLatchesIssuanceOffUntilRestart(t *testing.T) {
	hsm := newSigner(t)
	good := hsm.record(t, hardhatMnemonic)
	iss, err := NewIssuer(&hsm.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := iss.AddressFor(good, 0); err != nil || a != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" {
		t.Fatalf("issue on a verified record: %s %v", a, err)
	}
	attackerMn, _ := hdwallet.NewMnemonic()
	attacker, _ := hdwallet.AccountXPub(attackerMn)
	swapped := Record{Chain: ChainEVM, Path: PathEVM, XPub: attacker, Attestation: good.Attestation}
	if a, err := iss.AddressFor(swapped, 1); err == nil || a != "" {
		t.Fatalf("a swapped xpub issued %q", a)
	}
	if !iss.Latched() || iss.Reason() == nil {
		t.Fatal("the failure did not latch")
	}
	// latched: even the genuine record issues nothing now (no flapping back to open when the attacker restores the row)
	if a, err := iss.AddressFor(good, 2); err == nil || a != "" {
		t.Fatalf("issued %q while latched", a)
	}
	if err := iss.Check(good); err == nil {
		t.Fatal("Check passed while latched")
	}
	// a restart is a fresh Issuer and works again with the genuine record
	iss2, _ := NewIssuer(&hsm.key.PublicKey)
	if _, err := iss2.AddressFor(good, 2); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestParsePinned_Encodings(t *testing.T) {
	k := newSigner(t)
	raw := elliptic.Marshal(elliptic.P256(), k.key.X, k.key.Y) //nolint:staticcheck
	der, _ := x509.MarshalPKIXPublicKey(&k.key.PublicKey)
	pemB := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	for name, b := range map[string][]byte{
		"hex":             []byte(hex.EncodeToString(raw) + "\n"),
		"0x hex":          []byte("0x" + hex.EncodeToString(raw)),
		"init-token line": []byte("attest_public_key=" + hex.EncodeToString(raw) + "\n"),
		"pem":             pemB,
	} {
		pk, err := ParsePinned(b)
		if err != nil || PinnedHex(pk) != hex.EncodeToString(raw) {
			t.Errorf("%s: %v", name, err)
		}
	}
	secp := make([]byte, 65)
	secp[0] = 4
	for name, b := range map[string][]byte{
		"empty": nil, "short": []byte("04ab"), "not hex": []byte(strings.Repeat("zz", 65)),
		"off curve": []byte(hex.EncodeToString(secp)), "compressed": []byte(hex.EncodeToString(raw[:33])), "bad pem": []byte("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"),
	} {
		if _, err := ParsePinned(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	p := filepath.Join(t.TempDir(), "pin")
	_ = os.WriteFile(p, []byte(hex.EncodeToString(raw)), 0o600)
	if _, err := LoadPinned(p); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPinned(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing pin file was accepted")
	}
}

// The same vector is pinned in hsm-gateway/internal/attest: the two modules build the statement separately.
func TestGoldenVectorMatchesTheGateway(t *testing.T) {
	seed := make([]byte, 64)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	k, _ := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	for _, i := range []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart} {
		k, _ = k.Derive(i)
	}
	pub, _ := k.Neuter()
	const goldenXPub = "xpub6DQmpJ27MaP3AE4wd3kHuXHMYHqp6CsgkVj4P7QwJTGj44NYDxXLG9NPguvbzGvMSwzVjYEZPjufGVu4oKKfnt43RsDdRZVcnmiEpJ14BzD"
	const goldenStatement = "digiwal/xpub-attest/v1\nchain=evm\npath=m/44'/60'/0'\nxpub=" + goldenXPub + "\n"
	if pub.String() != goldenXPub || string(Statement(ChainEVM, PathEVM, goldenXPub)) != goldenStatement {
		t.Fatalf("statement drifted from the gateway's golden vector: %q", Statement(ChainEVM, PathEVM, pub.String()))
	}
	d := sha256.Sum256([]byte(goldenStatement))
	if got := Digest(ChainEVM, PathEVM, goldenXPub); got != d || hex.EncodeToString(d[:]) != "665269afc8e7d542db4908848b75311bafe69a07a6dab777e754cfd8d1e80a34" {
		t.Fatalf("digest %x", got)
	}
}

// Invariant 9: the issuing path holds no secret. Enforced structurally: this package imports no seed, sealing or
// software-signing code, so nothing here can open wallet_seeds or need MASTER_KEY_HEX.
func TestPackageImportsNoSeedOrSealingCode(t *testing.T) {
	forbidden := []string{"internal/hdwallet", "internal/crypto", "go-ethereum-hdwallet", "internal/signing", "go-bip39"}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			for _, bad := range forbidden {
				if strings.Contains(im.Path.Value, bad) {
					t.Errorf("%s imports %s", f, im.Path.Value)
				}
			}
		}
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"MASTER_KEY_HEX", "wallet_seeds", "Getenv"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s mentions %s", f, bad)
			}
		}
	}
}

// hdkeychain is pinned: a version bump is a reviewed go.mod change, not a drive-by (key-custody-hsm.md M3a).
func TestHDKeychainVersionIsPinned(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(b)
	if !strings.Contains(mod, "\tgithub.com/btcsuite/btcd/btcutil v1.1.5\n") {
		t.Fatal("go.mod does not pin github.com/btcsuite/btcd/btcutil v1.1.5 (hdkeychain)")
	}
	if strings.Contains(mod, "btcutil =>") {
		t.Fatal("btcutil is replaced")
	}
}
