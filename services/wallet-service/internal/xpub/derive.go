package xpub

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/ethereum/go-ethereum/crypto"
)

// ExternalChain is the BIP44 "external" chain node under the account: children are m/44'/60'/0'/0/{index}.
const ExternalChain = 0

// AddressAt derives the EIP-55 EVM address of m/44'/60'/0'/0/{index} from the account xpub by public derivation only
// (BIP32 CKDpub: non-hardened children of a public parent). A hardened index is an error: it cannot be derived from a public
// key, and index >= 2^31 never exists in this system.
func AddressAt(account *hdkeychain.ExtendedKey, index uint32) (string, error) {
	if index >= hdkeychain.HardenedKeyStart {
		return "", fmt.Errorf("derivation index %d is hardened: it cannot be derived from an xpub", index)
	}
	if account.IsPrivate() {
		return "", fmt.Errorf("refusing to derive from a private extended key")
	}
	ext, err := account.Derive(ExternalChain)
	if err != nil {
		return "", fmt.Errorf("deriving the external chain: %w", err)
	}
	child, err := ext.Derive(index)
	if err != nil {
		return "", fmt.Errorf("deriving index %d: %w", index, err)
	}
	pub, err := child.ECPubKey()
	if err != nil {
		return "", fmt.Errorf("index %d: %w", index, err)
	}
	return crypto.PubkeyToAddress(*pub.ToECDSA()).Hex(), nil
}
