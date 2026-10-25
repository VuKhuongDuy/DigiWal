package xpub

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// ErrUnavailable means no new deposit address may be issued: the attestation failed (latched until restart) or the
// pinned key / xpub row is missing. Existing addresses keep being served from stored rows; only creation is refused.
var ErrUnavailable = errors.New("deposit-address issuance unavailable")

// Issuer verifies the xpub Attestation on EVERY issuance and derives the child address from the key it verified.
// A verification failure latches issuance off in memory until the process restarts (key-custody-hsm.md invariant 10)
// and logs an ALERT; nothing is issued for a record that did not verify.
type Issuer struct {
	pin     *ecdsa.PublicKey
	latched atomic.Bool

	mu     sync.Mutex
	reason error
}

// NewIssuer returns an Issuer that trusts only attestations signed under pin.
func NewIssuer(pin *ecdsa.PublicKey) (*Issuer, error) {
	if pin == nil {
		return nil, errors.New("xpub issuer needs the pinned attestation public key")
	}
	return &Issuer{pin: pin}, nil
}

// Latched reports whether a verification failure has closed issuance.
func (i *Issuer) Latched() bool { return i.latched.Load() }

// Reason is the failure that latched issuance (nil while open).
func (i *Issuer) Reason() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reason
}

func (i *Issuer) latch(err error) {
	i.mu.Lock()
	first := i.reason == nil
	if first {
		i.reason = err
	}
	i.mu.Unlock()
	i.latched.Store(true)
	if first {
		log.Printf("ALERT xpub attestation failed, deposit-address issuance is OFF until restart (existing addresses still served): %v", err)
	}
}

// Check verifies rec against the pin (used at startup); a failure latches. A nil error means rec verified.
func (i *Issuer) Check(rec Record) error {
	if i.Latched() {
		return ErrUnavailable
	}
	if _, err := Verify(i.pin, rec); err != nil {
		i.latch(err)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// AddressFor verifies rec and derives the address at index from the key that was verified. It is the deriveFn of
// storage.CreateAddressFromXPub, so it runs inside the allocation transaction: a failure rolls the index back.
func (i *Issuer) AddressFor(rec Record, index uint32) (string, error) {
	if i.Latched() {
		return "", ErrUnavailable
	}
	acct, err := Verify(i.pin, rec)
	if err != nil {
		i.latch(err)
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return AddressAt(acct, index)
}
