// Package sigutil turns the raw r||s an HSM returns for a secp256k1 digest into the canonical
// (r, s, v) form Ethereum accepts. About half of raw ECDSA signatures have a high s (M0 spike
// (c): 49.7%), so normalisation is mandatory, not defensive; v is never trusted from the token
// (CKM_ECDSA returns none) but found by recovering against the pinned Hot address.
package sigutil

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	n     = crypto.S256().Params().N
	halfN = new(big.Int).Rsh(n, 1)

	// ErrNoRecovery means neither recovery id recovers the expected address: the key is not the
	// pinned one, or the token returned garbage. The caller must not return any signature.
	ErrNoRecovery = errors.New("sigutil: no recovery id yields the expected address")
	ErrMalformed  = errors.New("sigutil: malformed raw signature")
)

// Finalize returns the 65-byte [R || S || V] signature (V in {0,1}) for digest, with s in the
// lower half of the curve order, such that ecrecover(digest, sig) == want.
func Finalize(digest, rs []byte, want common.Address) ([]byte, error) {
	if len(digest) != 32 {
		return nil, fmt.Errorf("%w: digest is %d bytes", ErrMalformed, len(digest))
	}
	if len(rs) != 64 {
		return nil, fmt.Errorf("%w: raw signature is %d bytes", ErrMalformed, len(rs))
	}
	r := new(big.Int).SetBytes(rs[:32])
	s := new(big.Int).SetBytes(rs[32:])
	if r.Sign() == 0 || s.Sign() == 0 || r.Cmp(n) >= 0 || s.Cmp(n) >= 0 {
		return nil, fmt.Errorf("%w: r or s out of range", ErrMalformed)
	}
	if s.Cmp(halfN) > 0 {
		s.Sub(n, s)
	}
	sig := make([]byte, 65)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:64])
	for v := byte(0); v < 2; v++ {
		sig[64] = v
		pub, err := crypto.SigToPub(digest, sig)
		if err != nil {
			continue
		}
		if crypto.PubkeyToAddress(*pub) == want {
			return sig, nil
		}
	}
	return nil, ErrNoRecovery
}

// IsLowS reports whether the s component of a 65-byte signature is canonical.
func IsLowS(sig []byte) bool {
	return len(sig) == 65 && new(big.Int).SetBytes(sig[32:64]).Cmp(halfN) <= 0
}
