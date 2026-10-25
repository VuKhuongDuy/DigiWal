// Package derive turns the 64-byte Seed into a signature (key-custody-hsm.md invariant 14, K11): BIP32 private derivation of
// m/44'/60'/0'/0/{index} with btcd hdkeychain on []byte, an RFC 6979 low-s signature with btcec, the Ethereum
// (r, s, v) assembled by hand. It never makes a string out of the Seed: callers pass the bytes and nothing here has a
// mnemonic, BIP39 or go-ethereum-hdwallet import (a test pins that).
//
// Zeroing. Every derived node (master, 44', 60', 0', 0, the leaf), the *btcec.PrivateKey made from the leaf and the
// signature scratch are zeroed explicitly before the function returns, on every path, because hdkeychain's Zero() clears
// only the node it is called on (M0 spike (g)) and btcec's PrivateKey needs its own Zero(). This is best effort: hdkeychain
// copies the key into HMAC state and intermediate slices it does not expose, and the Go runtime may move stacks; those
// copies cannot be reached from here. The Seed itself lives in a seedpage.Page that the caller zeroes and unmaps.
package derive

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// The fixed prefix of the account tree: m/44'/60'/0' (hardened), then /0/{index} (non-hardened). The same tree the
// system has always used (invariant 12), so every issued derivation_index keeps its address.
var accountPath = []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart + 0}

// ExternalChain is the BIP44 external chain node under the account.
const ExternalChain = 0

// keys is every secret object one derivation creates, so one method zeroes all of them.
type keys struct {
	nodes []*hdkeychain.ExtendedKey
	priv  *btcec.PrivateKey
}

// zero clears every node and the private key. It is called from a defer, so a panic or an early error return still zeroes.
func (k *keys) zero() {
	for _, n := range k.nodes {
		if n != nil {
			n.Zero()
		}
	}
	k.nodes = nil
	if k.priv != nil {
		k.priv.Zero()
		k.priv = nil
	}
}

// step derives child i of parent and records it (and so its zeroing) in k.
func (k *keys) step(parent *hdkeychain.ExtendedKey, i uint32) (*hdkeychain.ExtendedKey, error) {
	c, err := parent.Derive(i)
	if c != nil {
		k.nodes = append(k.nodes, c)
	}
	return c, err
}

// master builds the master node from the Seed bytes.
func (k *keys) master(seed []byte) (*hdkeychain.ExtendedKey, error) {
	m, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if m != nil {
		k.nodes = append(k.nodes, m)
	}
	return m, err
}

// account derives the account node m/44'/60'/0'.
func (k *keys) account(seed []byte) (*hdkeychain.ExtendedKey, error) {
	n, err := k.master(seed)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	for _, i := range accountPath {
		if n, err = k.step(n, i); err != nil {
			return nil, fmt.Errorf("deriving the account node: %w", err)
		}
	}
	return n, nil
}

// external derives m/44'/60'/0'/0.
func (k *keys) external(seed []byte) (*hdkeychain.ExtendedKey, error) {
	a, err := k.account(seed)
	if err != nil {
		return nil, err
	}
	e, err := k.step(a, ExternalChain)
	if err != nil {
		return nil, fmt.Errorf("deriving the external chain: %w", err)
	}
	return e, nil
}

func checkSeed(seed []byte) error {
	if len(seed) != 64 {
		return fmt.Errorf("seed is %d bytes, want 64", len(seed))
	}
	return nil
}

func checkIndex(index uint32) error {
	if index >= hdkeychain.HardenedKeyStart {
		return fmt.Errorf("derivation index %d is hardened", index)
	}
	return nil
}

// AddressAt derives the EVM address at m/44'/60'/0'/0/{index}. Everything secret is zeroed before it returns.
func AddressAt(seed []byte, index uint32) (common.Address, error) {
	if err := checkSeed(seed); err != nil {
		return common.Address{}, err
	}
	if err := checkIndex(index); err != nil {
		return common.Address{}, err
	}
	var k keys
	defer k.zero()
	ext, err := k.external(seed)
	if err != nil {
		return common.Address{}, err
	}
	return addressOf(&k, ext, index)
}

// AddressesAt derives many addresses from one external-chain node (the Address Audit's pass), zeroing each child as soon as
// its address is read and the shared nodes at the end.
func AddressesAt(seed []byte, indexes []uint32) ([]common.Address, error) {
	if err := checkSeed(seed); err != nil {
		return nil, err
	}
	var k keys
	defer k.zero()
	ext, err := k.external(seed)
	if err != nil {
		return nil, err
	}
	out := make([]common.Address, len(indexes))
	for j, idx := range indexes {
		if err := checkIndex(idx); err != nil {
			return nil, err
		}
		child := keys{}
		a, err := addressOf(&child, ext, idx)
		child.zero()
		if err != nil {
			return nil, err
		}
		out[j] = a
	}
	return out, nil
}

func addressOf(k *keys, ext *hdkeychain.ExtendedKey, index uint32) (common.Address, error) {
	leaf, err := k.step(ext, index)
	if err != nil {
		return common.Address{}, fmt.Errorf("deriving index %d: %w", index, err)
	}
	pub, err := leaf.ECPubKey()
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub.ToECDSA()), nil
}

// AccountXPub is the account-level extended PUBLIC key m/44'/60'/0' of the Seed (what wallet_xpubs holds), for the Address
// Audit's xpub comparison. Only the public half is returned.
func AccountXPub(seed []byte) (string, error) {
	if err := checkSeed(seed); err != nil {
		return "", err
	}
	var k keys
	defer k.zero()
	acct, err := k.account(seed)
	if err != nil {
		return "", err
	}
	pub, err := acct.Neuter()
	if err != nil {
		return "", err
	}
	return pub.String(), nil
}

// ErrWrongKey is returned when the key derived at an index is not the address the caller expected: nothing was signed.
var ErrWrongKey = errors.New("the key derived at this index is not the expected address")

// SignTx derives the key at m/44'/60'/0'/0/{index}, requires its address to be want (the stored wallet_addresses row, so a
// swapped xpub is refused here, invariant 11), signs tx for chainID and checks the signature recovers to want. Every secret
// is zeroed before it returns, including on the error paths.
func SignTx(seed []byte, index uint32, want common.Address, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if err := checkSeed(seed); err != nil {
		return nil, err
	}
	if err := checkIndex(index); err != nil {
		return nil, err
	}
	var k keys
	defer k.zero()
	ext, err := k.external(seed)
	if err != nil {
		return nil, err
	}
	leaf, err := k.step(ext, index)
	if err != nil {
		return nil, fmt.Errorf("deriving index %d: %w", index, err)
	}
	pub, err := leaf.ECPubKey()
	if err != nil {
		return nil, err
	}
	if crypto.PubkeyToAddress(*pub.ToECDSA()) != want {
		return nil, ErrWrongKey
	}
	if k.priv, err = leaf.ECPrivKey(); err != nil {
		return nil, err
	}

	signer := types.LatestSignerForChainID(chainID)
	h := signer.Hash(tx)
	// SignCompact: RFC 6979 deterministic nonce, canonical low-s. compact = [27+recid | r(32) | s(32)].
	compact, err := ecdsa.SignCompact(k.priv, h[:], false)
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	defer clear(compact)
	sig := make([]byte, 65) // r || s || v with v in {0,1}, the form go-ethereum's WithSignature takes
	defer clear(sig)
	copy(sig[0:64], compact[1:65])
	sig[64] = compact[0] - 27
	signed, err := tx.WithSignature(signer, sig)
	if err != nil {
		return nil, fmt.Errorf("assembling the signature: %w", err)
	}
	if from, err := types.Sender(signer, signed); err != nil || from != want {
		return nil, fmt.Errorf("signature does not recover to the expected address: %v", err)
	}
	return signed, nil
}
