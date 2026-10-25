// Package seedkit is the seed code of key-admin's fresh initialisation (key-custody-hsm.md M3c): generate a BIP39 mnemonic, turn it into
// the 64-byte Seed, derive the account xpub and per-index addresses from the Seed bytes. It is deliberately NOT part of package hdwallet:
// hdwallet's seed and signer code is compiled out of a `-tags prod` build so the production wallet-service links no seed code at all
// (cmd/service/prod_test.go), while key-admin is a production ceremony tool and needs these few functions in every build.
// The derivation is the same hdkeychain walk as hdwallet and sweep-signer; seedkit_legacy_test.go pins the three to each other.
package seedkit

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/crypto"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"
)

// NewMnemonic generates a fresh 24-word BIP39 mnemonic (256 bits of entropy). Ceremony use only: shown once, never stored.
func NewMnemonic() (string, error) { return hdw.NewMnemonic(256) }

// SeedFromMnemonic is the 64-byte BIP39 seed of a mnemonic (empty passphrase).
func SeedFromMnemonic(mnemonic string) ([]byte, error) {
	seed, err := hdw.NewSeedFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("seed from mnemonic: %w", err)
	}
	return seed, nil
}

func zeroAll(nodes []*hdkeychain.ExtendedKey) {
	for _, n := range nodes {
		if n != nil {
			n.Zero()
		}
	}
}

func walk(seed []byte, path []uint32) (*hdkeychain.ExtendedKey, []*hdkeychain.ExtendedKey, error) {
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, nil, fmt.Errorf("master key: %w", err)
	}
	nodes := []*hdkeychain.ExtendedKey{k}
	for _, i := range path {
		if k, err = k.Derive(i); err != nil {
			return nil, nodes, err
		}
		nodes = append(nodes, k)
	}
	return k, nodes, nil
}

// AccountXPubFromSeed is the account xpub of the Seed (m/44'/60'/0').
func AccountXPubFromSeed(seed []byte) (string, error) {
	k, nodes, err := walk(seed, []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart})
	defer zeroAll(nodes)
	if err != nil {
		return "", err
	}
	pub, err := k.Neuter()
	if err != nil {
		return "", err
	}
	return pub.String(), nil
}

// AddressAtSeed is the EVM address at m/44'/60'/0'/0/{index}, derived privately from the Seed.
func AddressAtSeed(seed []byte, index uint32) (string, error) {
	k, nodes, err := walk(seed, []uint32{hdkeychain.HardenedKeyStart + 44, hdkeychain.HardenedKeyStart + 60, hdkeychain.HardenedKeyStart, 0, index})
	defer zeroAll(nodes)
	if err != nil {
		return "", err
	}
	pub, err := k.ECPubKey()
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(*pub.ToECDSA()).Hex(), nil
}
