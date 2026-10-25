// Package audit is the Address Audit (key-custody-hsm.md invariant 11): the seed holder checks the xpub issuer. wallet-service
// issues deposit addresses from an attested xpub it can verify but cannot have derived; sweep-signer holds the Seed, so it
// is the only place that can notice an xpub that does not belong to it (an attacker who controls the wallet-service host
// and the pinned key file can swap the xpub and survive invariant 10). The loop re-derives the issued addresses from the
// Seed and compares them with wallet_addresses, and the Seed's account xpub with wallet_xpubs. ONE mismatch freezes all
// signing (until restart), raises an ALERT and a metric. Until the first pass has completed, signing is refused too.
//
// Each pass opens the Seed once (a locked page, zeroed at the end of the pass), walks every row issued since the last pass
// and re-checks a rolling window of older rows, so over time every row is re-derived. A pass that fails for an
// infrastructure reason (database down, Seed not unsealable) is not a mismatch: it does not freeze, it is simply not a
// success, and the gate stays as it was.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"digiwal/services/sweep-signer/internal/derive"
	"digiwal/services/sweep-signer/internal/store"
)

// Directory is the read side the audit needs.
type Directory interface {
	IssuedAfter(ctx context.Context, chain string, after int64, limit int) ([]store.Issued, error)
	AccountXPub(ctx context.Context) (string, error)
}

// Seeds hands the Seed to fn for one call.
type Seeds interface {
	With(ctx context.Context, fn func(seed []byte) error) error
}

// Auditor is the audit loop and the signing gate.
type Auditor struct {
	dir   Directory
	seeds Seeds

	// Batch is how many rows one derivation call handles; Window how many already-verified rows are re-checked per pass.
	Batch  int
	Window int

	mu         sync.Mutex
	watermark  int64 // every index <= watermark has been verified at least once
	cursor     int64 // rolling re-check position
	passed     bool  // a full pass has completed without a mismatch
	frozen     string
	mismatches int
	lastOK     time.Time
	checked    uint64
	now        func() time.Time
	alert      func(string)
}

// New returns an Auditor with signing closed until the first pass succeeds.
func New(dir Directory, seeds Seeds) *Auditor {
	return &Auditor{dir: dir, seeds: seeds, Batch: 200, Window: 200, watermark: -1, cursor: -1, now: time.Now,
		alert: func(m string) { log.Print("ALERT " + m) }}
}

// Ready is nil only after a clean first pass and while no mismatch has been found.
func (a *Auditor) Ready() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.frozen != "":
		return errors.New("address audit mismatch: " + a.frozen)
	case !a.passed:
		return errors.New("the first address audit pass has not completed")
	}
	return nil
}

// Trip freezes signing (used by the Sweep path when a single derivation disagrees with the stored row).
func (a *Auditor) Trip(reason string) { a.freeze(reason) }

func (a *Auditor) freeze(reason string) {
	a.mu.Lock()
	first := a.frozen == ""
	if first {
		a.frozen = reason
	}
	a.mismatches++
	a.mu.Unlock()
	if first {
		a.alert("address audit: " + reason + ": ALL signing is refused until this service is restarted after investigation (a swapped xpub, or the wrong Seed)")
	}
}

// Frozen is the freeze reason ("" while open); Mismatches the number of mismatches seen; Checked the rows re-derived so far;
// LastOK the time of the last clean pass.
func (a *Auditor) Frozen() string    { a.mu.Lock(); defer a.mu.Unlock(); return a.frozen }
func (a *Auditor) Mismatches() int   { a.mu.Lock(); defer a.mu.Unlock(); return a.mismatches }
func (a *Auditor) Checked() uint64   { a.mu.Lock(); defer a.mu.Unlock(); return a.checked }
func (a *Auditor) LastOK() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.lastOK }

// RunOnce is one pass. It returns an error for infrastructure failures; a mismatch freezes and returns nil (the alert is raised).
func (a *Auditor) RunOnce(ctx context.Context) error {
	a.mu.Lock()
	wm, cur := a.watermark, a.cursor
	a.mu.Unlock()

	// Everything this pass will verify: all rows after the watermark, then a window after the rolling cursor.
	var fresh, older []store.Issued
	for after := wm; ; {
		rows, err := a.dir.IssuedAfter(ctx, store.ChainEVM, after, a.Batch)
		if err != nil {
			return err
		}
		fresh = append(fresh, rows...)
		if len(rows) < a.Batch {
			break
		}
		after = int64(rows[len(rows)-1].Index)
	}
	if wm >= 0 && a.Window > 0 {
		var err error
		if older, err = a.dir.IssuedAfter(ctx, store.ChainEVM, cur, a.Window); err != nil {
			return err
		}
		// keep only rows already behind the watermark (the fresh ones are checked anyway)
		n := 0
		for _, r := range older {
			if int64(r.Index) <= wm {
				older[n] = r
				n++
			}
		}
		older = older[:n]
	}
	stored, err := a.dir.AccountXPub(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	haveXPub := err == nil

	rows := append(append([]store.Issued(nil), fresh...), older...)
	indexes := make([]uint32, len(rows))
	for i, r := range rows {
		indexes[i] = r.Index
	}

	var mismatch string
	err = a.seeds.With(ctx, func(seed []byte) error {
		if haveXPub {
			got, err := derive.AccountXPub(seed)
			if err != nil {
				return err
			}
			if got != stored {
				mismatch = "the account xpub in wallet_xpubs is not the account xpub of the Seed"
				return nil
			}
		}
		for start := 0; start < len(indexes) && mismatch == ""; start += a.Batch {
			end := min(start+a.Batch, len(indexes))
			derived, err := derive.AddressesAt(seed, indexes[start:end])
			if err != nil {
				return err
			}
			for j, d := range derived {
				r := rows[start+j]
				if !strings.EqualFold(d.Hex(), r.Address) {
					mismatch = fmt.Sprintf("wallet_addresses index %d holds %s but the Seed derives %s", r.Index, r.Address, d.Hex())
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if mismatch != "" {
		a.freeze(mismatch)
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.checked += uint64(len(rows))
	if n := len(fresh); n > 0 {
		a.watermark = int64(fresh[n-1].Index)
	}
	if len(older) > 0 {
		a.cursor = int64(older[len(older)-1].Index)
	} else {
		a.cursor = -1 // wrapped: start the window again from the beginning
	}
	a.passed = true
	a.lastOK = a.now()
	return nil
}

// Run is the loop: one pass now, then every interval, until ctx is done. Failed passes are logged and retried.
func (a *Auditor) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("address audit pass failed (no verdict, signing gate unchanged): %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
