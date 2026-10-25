//go:build !prod

// Package hdwallet is the Software Signer: the software signing backend (dev and
// unit tests only; key-custody-hsm.md invariant 7). Every file here except index.go
// is excluded from a `-tags prod` build, so the production binary contains no
// software key code. It wraps standard, audited libraries (go-ethereum +
// BIP32/39/44) for the one master seed this system derives every user
// deposit address from. Deliberately does not implement any of the
// elliptic-curve or key-derivation math itself — that's exactly the
// kind of code you don't hand-roll for something holding funds, even
// testnet funds.
package hdwallet

import (
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"
)

// EVM accounts are all derived under the standard Ethereum BIP44
// coin type (60), account 0, external chain (0): m/44'/60'/0'/0/{index}.
// This is why one EVM address works across every EVM chain (Ethereum,
// Polygon, BSC, Arbitrum, ...) — the derivation path doesn't encode a
// specific chain, just "Ethereum-style keys".
const pathTemplate = "m/44'/60'/0'/0/%d"

// NewMnemonic generates a fresh BIP39 mnemonic (24 words / 256 bits of
// entropy). Call this exactly once per system; shown to the operator,
// never persisted in plaintext, never returned by any API again.
func NewMnemonic() (string, error) {
	return hdw.NewMnemonic(256)
}

// Wallet derives addresses and signs with them, given the (decrypted,
// in-memory-only) mnemonic. Never persisted; reconstructed from the
// sealed seed on each process start and held only in memory.
type Wallet struct {
	w *hdw.Wallet
}

func FromMnemonic(mnemonic string) (*Wallet, error) {
	w, err := hdw.NewFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("deriving wallet from mnemonic: %w", err)
	}
	return &Wallet{w: w}, nil
}

// SeedFromMnemonic is the 64-byte BIP39 seed of a mnemonic (empty passphrase, the go-ethereum-hdwallet default). Ceremony use
// only: this is the one place a mnemonic string becomes the Seed (key-custody-hsm.md K11).
func SeedFromMnemonic(mnemonic string) ([]byte, error) {
	seed, err := hdw.NewSeedFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("seed from mnemonic: %w", err)
	}
	return seed, nil
}

// AccountXPub is the account-level extended PUBLIC key at m/44'/60'/0' for a mnemonic (key-custody-hsm.md invariant 12):
// the one value `key-admin migrate-xpub` hands to the HSM for attestation. Only the public half leaves this function.
func AccountXPub(mnemonic string) (string, error) {
	seed, err := SeedFromMnemonic(mnemonic)
	if err != nil {
		return "", err
	}
	defer clear(seed)
	return AccountXPubFromSeed(seed)
}

// zeroAll clears every node it is given (hdkeychain's Zero() covers only the node it is called on).
func zeroAll(nodes []*hdkeychain.ExtendedKey) {
	for _, n := range nodes {
		if n != nil {
			n.Zero()
		}
	}
}

// walk derives the given child numbers from the master key of seed and returns the final node plus every node created.
func walk(seed []byte, path []uint32) (*hdkeychain.ExtendedKey, []*hdkeychain.ExtendedKey, error) {
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	var nodes []*hdkeychain.ExtendedKey
	if k != nil {
		nodes = append(nodes, k)
	}
	if err != nil {
		return nil, nodes, fmt.Errorf("master key: %w", err)
	}
	for _, i := range path {
		if k, err = k.Derive(i); err != nil {
			return nil, nodes, fmt.Errorf("deriving child %d: %w", i, err)
		}
		nodes = append(nodes, k)
	}
	return k, nodes, nil
}

// AccountXPubFromSeed is the account xpub of the 64-byte Seed (m/44'/60'/0'), derived with hdkeychain on bytes.
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

// AddressAtSeed derives the EVM address at m/44'/60'/0'/0/{index} from the 64-byte Seed with hdkeychain on bytes: the same
// derivation sweep-signer uses to sign, so `key-admin convert-seed` can prove the converted Seed reproduces every issued address.
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

// AddressAt derives the EVM address at the given BIP44 index. Purely
// deterministic — deriving the same index twice always yields the same
// address, which is what lets wallet-service be stateless about the
// derivation itself (Postgres just remembers which index belongs to
// which user).
func (hw *Wallet) AddressAt(index uint32) (string, error) {
	path := hdw.MustParseDerivationPath(fmt.Sprintf(pathTemplate, index))
	account, err := hw.w.Derive(path, false)
	if err != nil {
		return "", fmt.Errorf("deriving index %d: %w", index, err)
	}
	return account.Address.Hex(), nil
}

// SignTxAt signs an EIP-1559 transaction with the key at the given BIP44 index.
// The private key never leaves this package.
func (hw *Wallet) SignTxAt(index uint32, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	path := hdw.MustParseDerivationPath(fmt.Sprintf(pathTemplate, index))
	// The library only signs for accounts pinned to the wallet; pinning is
	// idempotent per address and holds no key material beyond the seed.
	account, err := hw.w.Derive(path, true)
	if err != nil {
		return nil, fmt.Errorf("deriving index %d: %w", index, err)
	}
	return hw.w.SignTxEIP1559(account, tx, chainID)
}
