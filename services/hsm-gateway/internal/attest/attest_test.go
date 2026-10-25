package attest

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

// testSeed is a fixed, throwaway 64-byte seed (never a real one): the golden vector below is derived from it.
func testSeed() []byte {
	s := make([]byte, 64)
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

func account(t *testing.T, path ...uint32) *hdkeychain.ExtendedKey {
	t.Helper()
	k, err := hdkeychain.NewMaster(testSeed(), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range path {
		if k, err = k.Derive(i); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

func accountXPub(t *testing.T) string {
	k := account(t, hdkeychain.HardenedKeyStart+44, hdkeychain.HardenedKeyStart+60, hdkeychain.HardenedKeyStart)
	n, err := k.Neuter()
	if err != nil {
		t.Fatal(err)
	}
	return n.String()
}

// The same golden vector is pinned in services/wallet-service/internal/xpub: the two modules build the statement
// separately, so a drift in either one fails its own test.
const (
	goldenXPub      = "xpub6DQmpJ27MaP3AE4wd3kHuXHMYHqp6CsgkVj4P7QwJTGj44NYDxXLG9NPguvbzGvMSwzVjYEZPjufGVu4oKKfnt43RsDdRZVcnmiEpJ14BzD"
	goldenStatement = "digiwal/xpub-attest/v1\nchain=evm\npath=m/44'/60'/0'\nxpub=" + goldenXPub + "\n"
	goldenDigest    = "665269afc8e7d542db4908848b75311bafe69a07a6dab777e754cfd8d1e80a34" // shasum -a 256 of goldenStatement
)

func TestGoldenVector(t *testing.T) {
	if got := accountXPub(t); got != goldenXPub {
		t.Fatalf("test seed's account xpub = %s", got)
	}
	q := &Request{Chain: ChainEVM, Path: PathEVM, XPub: goldenXPub}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
	if string(q.Statement()) != goldenStatement {
		t.Fatalf("statement drifted:\n%q", q.Statement())
	}
	d := q.Digest()
	if hex.EncodeToString(d[:]) != goldenDigest {
		t.Fatalf("digest %x", d)
	}
}

func TestValidate_RefusesEveryUnknownShape(t *testing.T) {
	good := accountXPub(t)
	priv := account(t, hdkeychain.HardenedKeyStart+44, hdkeychain.HardenedKeyStart+60, hdkeychain.HardenedKeyStart).String()
	root, _ := account(t).Neuter()
	acct := account(t, hdkeychain.HardenedKeyStart+44, hdkeychain.HardenedKeyStart+60, hdkeychain.HardenedKeyStart)
	external, _ := acct.Derive(0) // depth 4: the chain node, not the account node
	externalPub, _ := external.Neuter()
	acct1 := account(t, hdkeychain.HardenedKeyStart+44, hdkeychain.HardenedKeyStart+60, hdkeychain.HardenedKeyStart+1)
	acct1Pub, _ := acct1.Neuter()
	testnet, _ := hdkeychain.NewMaster(testSeed(), &chaincfg.TestNet3Params)
	testnetPub, _ := testnet.Neuter()

	if err := (&Request{Chain: "evm", Path: PathEVM, XPub: good}).Validate(); err != nil {
		t.Fatalf("the allowed shape was refused: %v", err)
	}
	for name, q := range map[string]Request{
		"solana":            {Chain: "solana", Path: PathEVM, XPub: good},
		"empty chain":       {Chain: "", Path: PathEVM, XPub: good},
		"bitcoin":           {Chain: "bitcoin", Path: "m/44'/0'/0'", XPub: good},
		"other path":        {Chain: "evm", Path: "m/44'/60'/1'", XPub: good},
		"path with child":   {Chain: "evm", Path: "m/44'/60'/0'/0", XPub: good},
		"path unhardened":   {Chain: "evm", Path: "m/44/60/0", XPub: good},
		"private key":       {Chain: "evm", Path: PathEVM, XPub: priv},
		"root xpub":         {Chain: "evm", Path: PathEVM, XPub: root.String()},
		"depth 4":           {Chain: "evm", Path: PathEVM, XPub: externalPub.String()},
		"account 1":         {Chain: "evm", Path: PathEVM, XPub: acct1Pub.String()},
		"testnet tpub":      {Chain: "evm", Path: PathEVM, XPub: testnetPub.String()},
		"garbage":           {Chain: "evm", Path: PathEVM, XPub: "xpub-nonsense"},
		"empty xpub":        {Chain: "evm", Path: PathEVM},
		"newline injection": {Chain: "evm", Path: PathEVM, XPub: good + "\nchain=solana"},
	} {
		q := q
		err := q.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "refused statement shape") {
			t.Errorf("%s: error is not ErrShape: %v", name, err)
		}
	}
	// account 1 differs from account 0 only in the child number: the attestation cannot be moved between accounts.
	if acct1Pub.String() == good {
		t.Fatal("test setup")
	}
}

func TestDecode_StrictJSON(t *testing.T) {
	ok := `{"chain":"evm","path":"m/44'/60'/0'","xpub":"x"}`
	if _, err := Decode(strings.NewReader(ok)); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{
		"unknown field":   `{"chain":"evm","path":"p","xpub":"x","digest":"0x00"}`,
		"signature field": `{"chain":"evm","path":"p","xpub":"x","signature":"00"}`,
		"trailing object": ok + ok,
		"trailing junk":   ok + " x",
		"not json":        `{`,
		"array":           `[]`,
		"empty":           ``,
	} {
		if _, err := Decode(strings.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
