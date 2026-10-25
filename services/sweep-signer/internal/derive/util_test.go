package derive

import (
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"
)

func keccak(b []byte) []byte { return crypto.Keccak256(b) }

func secp256k1N() *big.Int { return crypto.S256().Params().N }
