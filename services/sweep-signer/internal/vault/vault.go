// Package vault gets the user_derivation Seed for one call (key-custody-hsm.md invariants 14, 17, 18, M3c). The Seed is NOT opened here:
// the active Sealed Secret row (ciphertext, wrapped DEK, AAD) is read from the database and sent to hsm-gateway, which unwraps the DEK with
// the KEK that lives in the HSM, decrypts inside the HSM and returns the 64 plaintext bytes over mTLS. This process holds no key at all:
// no master key, no KEK, no DEK. The plaintext is read from the response straight into one dedicated locked page (seedpage) and the page is
// overwritten and unmapped when the call ends. If the gateway cannot be reached, answers anything but the seed, or says "authentication
// failed", there is no Seed and therefore no signature: fail closed, no fallback of any kind.
package vault

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"digiwal/services/sweep-signer/internal/seedpage"
)

// Row is the active Sealed Secret of the user_derivation role as stored (vault.active_secrets).
type Row struct {
	Role       string
	Purpose    string
	Version    int
	KEKVersion int
	WrappedDEK []byte
	Nonce      []byte
	Ciphertext []byte
	AAD        []byte
}

// Source is where the active row comes from (the database).
type Source interface {
	ActiveSecret(ctx context.Context) (Row, error)
}

// Unsealer asks hsm-gateway to open a row and writes the plaintext into dst (the locked page), returning how many bytes it wrote.
// It never returns the plaintext any other way.
type Unsealer interface {
	UnsealInto(ctx context.Context, row Row, dst []byte) (int, error)
}

// ErrSeedUnavailable: there is no usable Seed (no active row, the database or the gateway is down, or the gateway refused to open it).
var ErrSeedUnavailable = errors.New("seed unavailable")

// ErrAuthFailed: the gateway says the row does not authenticate (tampered row, wrong AAD, wrong KEK). Never retried, always an alert.
var ErrAuthFailed = errors.New("sealed seed does not authenticate (hsm-gateway: authentication failed)")

// Provider hands the Seed to a callback for the duration of one call. Per invariant 14 the Seed is unsealed per request, not cached: each
// With reads the active row, has the gateway open it into a fresh locked page, runs fn, then overwrites and unmaps the page, even if fn
// panics or fails.
type Provider struct {
	Source   Source
	Unsealer Unsealer
	// Strict makes an unlockable page (mlock refused) an error. Off by default: a warning-level fact, see Unlocked().
	Strict bool

	unseals  atomic.Uint64
	unlocked atomic.Uint64
	failures atomic.Uint64
}

// Unseals counts how many times a Seed was opened (a metric: it should track Sweeps plus audit passes, nothing else).
func (p *Provider) Unseals() uint64 { return p.unseals.Load() }

// Unlocked counts pages that could not be mlock'd.
func (p *Provider) Unlocked() uint64 { return p.unlocked.Load() }

// Failures counts calls that ended without a Seed (gateway down, refusal, tamper).
func (p *Provider) Failures() uint64 { return p.failures.Load() }

// With runs fn with the Seed. fn must not keep the slice: it is zeroed when With returns.
func (p *Provider) With(ctx context.Context, fn func(seed []byte) error) (err error) {
	row, err := p.Source.ActiveSecret(ctx)
	if err != nil {
		p.failures.Add(1)
		return fmt.Errorf("%w: %v", ErrSeedUnavailable, err)
	}
	page, err := seedpage.New(p.Strict)
	if err != nil {
		p.failures.Add(1)
		return fmt.Errorf("%w: %v", ErrSeedUnavailable, err)
	}
	defer page.Release() // the overwrite and the munmap, on every path including a panic in fn
	if !page.Locked() {
		p.unlocked.Add(1)
	}
	n, err := p.Unsealer.UnsealInto(ctx, row, page.Buffer())
	if err != nil {
		p.failures.Add(1)
		return fmt.Errorf("%w: %w", ErrSeedUnavailable, err)
	}
	if n != seedpage.Size {
		clear(page.Buffer())
		p.failures.Add(1)
		return fmt.Errorf("%w: hsm-gateway returned %d bytes, want %d", ErrSeedUnavailable, n, seedpage.Size)
	}
	p.unseals.Add(1)
	return fn(page.Buffer()[:n])
}
