package signing

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Operation classes the gateway's Signing Policy distinguishes (key-custody-hsm.md invariant 4). A
// Fee Bump is signed under the class of the transaction it replaces (the gateway detects a
// replacement by a nonce it has already signed); a Nonce Filler is a cancel.
const (
	OpPayout        = "payout"
	OpTreasurySweep = "treasury_sweep"
	OpCancel        = "cancel"
)

// HotMeta is what a Hot signature is requested under. The software backend ignores it; the gateway
// audits it and applies the policy class.
type HotMeta struct {
	Operation      string // OpPayout | OpTreasurySweep | OpCancel
	IdempotencyKey string
	Caller         string // the verified certificate CN of whoever asked, "" for the operator CLI
}

// HotBackend is the Hot Wallet's signing key as the signing package sees it (key-custody-hsm.md 5.8):
// one pinned address and "sign this transaction". The private key never crosses it. The software
// backend wraps a Backend at the Hot index (dev and tests); the gateway backend calls hsm-gateway.
type HotBackend interface {
	// HotAddress is the Hot Wallet address, read once and pinned by New.
	HotAddress(ctx context.Context) (common.Address, error)
	// SignHotTx signs tx (a DynamicFeeTx) for chainID. Whatever it returns is re-checked by the
	// caller (sender == pinned address) before it is stored.
	SignHotTx(ctx context.Context, tx *types.Transaction, chainID *big.Int, meta HotMeta) (*types.Transaction, error)
	// Available is checked BEFORE the DB lock is taken: a non-nil error (wrapping
	// ErrSignerUnavailable) means signing is known to be down and the request fails fast.
	Available() error
}

// Errors the HTTP layer maps: PolicyRefusedError to 422 policy_refused, ErrSignerUnavailable to 503.
var (
	// ErrSignerUnavailable wraps every failure to obtain a signature (gateway down, HSM error, circuit
	// breaker open, a signature that does not recover to the Hot address). No nonce was consumed.
	ErrSignerUnavailable = errors.New("hot key signer unavailable")
	// ErrSenderMismatch: the signature's recovered sender is not the pinned Hot address. It is also
	// ErrSignerUnavailable: the transaction is refused, never stored.
	ErrSenderMismatch = fmt.Errorf("%w: signature does not recover to the pinned Hot address", ErrSignerUnavailable)
)

// PolicyRefusedError is the gateway's 403 policy_refused: nothing was signed, Rule is the rule id.
type PolicyRefusedError struct{ Rule string }

func (e *PolicyRefusedError) Error() string { return "signing policy refused: " + e.Rule }

// softwareHot adapts a Backend at one BIP44 index to HotBackend (dev profile, tests).
type softwareHot struct {
	b     Backend
	index uint32
}

// SoftwareHot is the Hot backend over a software Backend at index.
func SoftwareHot(b Backend, index uint32) HotBackend { return softwareHot{b: b, index: index} }

func (s softwareHot) HotAddress(context.Context) (common.Address, error) {
	a, err := s.b.AddressAt(s.index)
	if err != nil {
		return common.Address{}, err
	}
	return common.HexToAddress(a), nil
}

func (s softwareHot) SignHotTx(_ context.Context, tx *types.Transaction, chainID *big.Int, _ HotMeta) (*types.Transaction, error) {
	return s.b.SignTxAt(s.index, tx, chainID)
}

func (softwareHot) Available() error { return nil }

// pinnedChain is implemented by a backend that knows which chain its key serves (the gateway's
// policy pins one); New refuses a mismatch with the RPC.
type pinnedChain interface{ PinnedChainID() uint64 }
