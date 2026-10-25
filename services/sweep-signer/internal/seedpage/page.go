// Package seedpage holds the user_derivation Seed (64 raw bytes, key-custody-hsm.md K11) in ONE dedicated page of
// anonymous memory outside the Go heap: mmap'd, mlock'd where the OS and RLIMIT_MEMLOCK allow, excluded from core dumps
// (MADV_DONTDUMP on Linux), overwritten with zeros and unmapped as soon as the request that needed it is done.
//
// What this does and does not give (stated plainly, invariant 14):
//
//   - The plaintext is written straight into the page by the AEAD (the decrypt destination IS the page), so the seed does
//     not pass through a Go string and the decrypt path makes no heap copy of it.
//   - mlock keeps the page out of swap when the limit allows it; if it does not, Page reports Locked() == false and the
//     service logs it (REQUIRE_MLOCK=1 turns that into a refusal to start).
//   - Everything downstream (hdkeychain's HMAC temporaries, the Go runtime's stack copying, the kernel's own buffers) can
//     leave copies this package cannot reach. Zeroing is best-effort. The real controls are the container (read-only, no
//     capabilities, no ptrace, no core, no swap), the short lifetime (one request, or one audit pass) and the tiny process.
package seedpage

import (
	"errors"
	"fmt"
	"os"
)

// Size is the Seed length: a BIP39 seed, 64 bytes.
const Size = 64

// Page is one dedicated page holding at most Size bytes of Seed.
type Page struct {
	mem    []byte // the whole mapping (a page, or more)
	locked bool

	released      bool
	zeroedOnUnmap bool // set by Release: every byte of the mapping was zero right before it was unmapped
}

// Buffer is the Size-byte window for the Seed. The AEAD decrypts into it (dst = Buffer()[:0]).
func (p *Page) Buffer() []byte {
	if p.released {
		return nil
	}
	return p.mem[:Size]
}

// Locked is whether mlock succeeded (the page cannot be swapped out).
func (p *Page) Locked() bool { return p.locked }

// ZeroedOnRelease is true after Release if the whole mapping read back as zero just before it was unmapped (a test and
// audit hook: it is the check that the overwrite really happened).
func (p *Page) ZeroedOnRelease() bool { return p.zeroedOnUnmap }

// Release overwrites the whole page with zeros, verifies that, then unlocks and unmaps it. Safe to call twice.
func (p *Page) Release() {
	if p == nil || p.released {
		return
	}
	p.released = true
	for i := range p.mem {
		p.mem[i] = 0
	}
	zero := true
	for _, b := range p.mem {
		if b != 0 {
			zero = false
		}
	}
	p.zeroedOnUnmap = zero
	p.unmap()
	p.mem = nil
}

// ErrNotLocked is returned by New when strict is set and mlock did not succeed.
var ErrNotLocked = errors.New("seedpage: mlock failed (RLIMIT_MEMLOCK too small or no CAP_IPC_LOCK)")

// New maps one fresh page and tries to lock it. With strict, a page that cannot be locked is an error instead of a
// warning-level fact the caller can read from Locked().
func New(strict bool) (*Page, error) {
	p, err := newPage()
	if err != nil {
		return nil, fmt.Errorf("seedpage: %w", err)
	}
	if strict && !p.locked {
		p.Release()
		return nil, ErrNotLocked
	}
	return p, nil
}

// pageSize is the system page size.
func pageSize() int {
	n := os.Getpagesize()
	if n < Size {
		return Size
	}
	return n
}
