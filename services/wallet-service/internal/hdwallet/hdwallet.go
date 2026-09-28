// Package hdwallet wraps standard, audited libraries (go-ethereum +
// BIP32/39/44) for the one master seed this system derives every user
// deposit address from. Deliberately does not implement any of the
// elliptic-curve or key-derivation math itself — that's exactly the
// kind of code you don't hand-roll for something holding funds, even
// testnet funds.
package hdwallet

import (
	"fmt"

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
