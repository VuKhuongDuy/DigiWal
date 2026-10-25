package signing

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// Backend is what the signing package needs from whatever holds the keys
// (key-custody-hsm.md section 5.8): an address lookup and sign-tx-at-index. It is
// the only thing here that knows how a key is held. The software backend is
// hdwallet.Wallet (dev and tests, absent from `-tags prod` builds); the PKCS#11
// backend arrives in a later milestone. The private key never crosses this interface.
type Backend interface {
	// AddressAt returns the EVM address (hex) of the key at the BIP44 index.
	AddressAt(index uint32) (string, error)
	// SignTxAt signs an EIP-1559 transaction with the key at the BIP44 index.
	SignTxAt(index uint32, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}
