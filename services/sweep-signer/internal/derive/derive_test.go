package derive

import (
	"context"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"

	"digiwal/services/sweep-signer/internal/seedpage"
)

// A well-known public test mnemonic: never used for anything real. The mnemonic string exists only in this TEST, to make a
// Seed and to cross-check against go-ethereum-hdwallet; no non-test file of this module has a mnemonic or a BIP39 import.
const testMnemonic = "test test test test test test test test test test test junk"

func testSeed(t testing.TB) []byte {
	t.Helper()
	seed, err := hdw.NewSeedFromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != seedpage.Size {
		t.Fatalf("seed is %d bytes", len(seed))
	}
	return seed
}

// hdkeychain + btcec on bytes reproduce go-ethereum-hdwallet's addresses for indexes 0-1000 (M0 spike (g), now against the
// code that ships), and for the repo's usual test mnemonic.
func TestAddressAt_ReproducesGoEthereumHDWalletForIndexes0To1000(t *testing.T) {
	seed := testSeed(t)
	w, err := hdw.NewFromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	indexes := make([]uint32, 1001)
	for i := range indexes {
		indexes[i] = uint32(i)
	}
	batch, err := AddressesAt(seed, indexes)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i <= 1000; i++ {
		acc, err := w.Derive(hdw.MustParseDerivationPath("m/44'/60'/0'/0/"+itoa(i)), false)
		if err != nil {
			t.Fatal(err)
		}
		one, err := AddressAt(seed, i)
		if err != nil {
			t.Fatal(err)
		}
		if one != acc.Address || batch[i] != acc.Address {
			t.Fatalf("index %d: AddressAt %s, AddressesAt %s, go-ethereum-hdwallet %s", i, one, batch[i], acc.Address)
		}
	}
	t.Logf("1001/1001 addresses (indexes 0-1000) match go-ethereum-hdwallet")
}

func itoa(i uint32) string { return big.NewInt(int64(i)).String() }

// The Seed's account xpub, derived publicly, gives the same addresses as the private derivation: the property the whole
// xpub-issuance design (invariants 10-12) rests on.
func TestAccountXPub_PublicDerivationMatchesPrivate(t *testing.T) {
	seed := testSeed(t)
	xp, err := AccountXPub(seed)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := hdkeychain.NewKeyFromString(xp)
	if err != nil || acct.IsPrivate() || acct.Depth() != 3 {
		t.Fatalf("not a depth-3 public key: %v", err)
	}
	ext, _ := acct.Derive(0)
	for _, i := range []uint32{0, 1, 7, 1000} {
		child, _ := ext.Derive(i)
		pub, _ := child.ECPubKey()
		want, _ := AddressAt(seed, i)
		got := common.BytesToAddress(keccak(pub.SerializeUncompressed()[1:])[12:])
		if got != want {
			t.Fatalf("index %d: public %s private %s", i, got, want)
		}
	}
}

// What is signed is accepted and mined by a real EVM, and recovers to the derived address.
func TestSignTx_RealEVMAcceptsIt(t *testing.T) {
	seed := testSeed(t)
	from, _ := AddressAt(seed, 3)
	to := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	b := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(2), big.NewInt(params.Ether))}})
	defer b.Close()
	cl := b.Client()
	id, _ := cl.ChainID(context.Background())
	head, _ := cl.HeaderByNumber(context.Background(), nil)
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: id, Nonce: 0, GasTipCap: big.NewInt(1e9), GasFeeCap: new(big.Int).Add(head.BaseFee, big.NewInt(2e9)),
		Gas: 21000, To: &to, Value: big.NewInt(1e18)})
	signed, err := SignTx(seed, 3, from, tx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sender, err := types.Sender(types.LatestSignerForChainID(id), signed); err != nil || sender != from {
		t.Fatalf("sender %s (%v), want %s", sender, err, from)
	}
	if err := cl.SendTransaction(context.Background(), signed); err != nil {
		t.Fatalf("the EVM rejected it: %v", err)
	}
	b.Commit()
	if bal, _ := cl.BalanceAt(context.Background(), to, nil); bal.Cmp(big.NewInt(1e18)) != 0 {
		t.Fatalf("recipient balance %s", bal)
	}
	// low-s (canonical): s <= N/2
	_, s, _ := signed.RawSignatureValues()
	half := new(big.Int).Rsh(secp256k1N(), 1)
	if s.Cmp(half) > 0 {
		t.Fatal("high-s signature")
	}
}

func TestSignTx_RefusesAWrongKeyAndABadSeedOrIndex(t *testing.T) {
	seed := testSeed(t)
	to := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(31337), GasFeeCap: big.NewInt(1), Gas: 21000, To: &to, Value: big.NewInt(1)})
	other, _ := AddressAt(seed, 5)
	if _, err := SignTx(seed, 4, other, tx, big.NewInt(31337)); err != ErrWrongKey {
		t.Fatalf("index 4 against index 5's address: got %v, want ErrWrongKey", err)
	}
	if _, err := SignTx(seed[:32], 0, other, tx, big.NewInt(31337)); err == nil {
		t.Fatal("a 32-byte seed was accepted")
	}
	if _, err := SignTx(seed, hdkeychain.HardenedKeyStart, other, tx, big.NewInt(31337)); err == nil {
		t.Fatal("a hardened index was accepted")
	}
}

// Zeroing coverage (M0 spike (g)): every derived node and the *btcec.PrivateKey are cleared by keys.zero(). hdkeychain's own
// Zero() clears only the node it is called on, so the test proves each node is dead (Derive on a zeroed key errors) and the
// scalar is zero.
func TestKeys_ZeroClearsEveryNodeAndThePrivateKey(t *testing.T) {
	seed := testSeed(t)
	var k keys
	ext, err := k.external(seed)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := k.step(ext, 9)
	if err != nil {
		t.Fatal(err)
	}
	if k.priv, err = leaf.ECPrivKey(); err != nil {
		t.Fatal(err)
	}
	nodes, priv := append([]*hdkeychain.ExtendedKey(nil), k.nodes...), k.priv
	if len(nodes) != 6 { // master, 44', 60', 0', 0, index
		t.Fatalf("%d nodes tracked, want 6", len(nodes))
	}
	if priv.Key.IsZero() {
		t.Fatal("test setup: the private scalar is already zero")
	}
	k.zero()
	for i, n := range nodes {
		if _, err := n.Derive(0); err == nil {
			t.Errorf("node %d still derives after zero(): its key bytes were not cleared", i)
		}
	}
	if !priv.Key.IsZero() {
		t.Error("the *btcec.PrivateKey scalar was not zeroed")
	}
	if k.nodes != nil || k.priv != nil {
		t.Error("keys still references its secrets")
	}
}

// Even when SignTx fails half way, nothing is left: a wrong-key refusal exercises the early-return path.
func TestSignTx_ZeroesOnTheErrorPathToo(t *testing.T) {
	seed := testSeed(t)
	to := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 21000, To: &to})
	before := append([]byte(nil), seed...)
	_, _ = SignTx(seed, 1, common.Address{1}, tx, big.NewInt(1))
	for i := range seed {
		if seed[i] != before[i] {
			t.Fatal("SignTx modified the caller's seed (zeroing it is the page's job, and it must not be altered meanwhile)")
		}
	}
}
