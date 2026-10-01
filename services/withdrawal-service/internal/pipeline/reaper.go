package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

// Failure reasons the Reaper records on a `failed` row.
const (
	ReasonStaleRequest      = "stale_request"       // stuck in `requested`: nothing was ever held
	ReasonStaleReservation  = "stale_reservation"   // stuck holding a Lock: it was Released
	ReasonReplacedOrDropped = "replaced_or_dropped" // its nonce was consumed by another tx; none of ours can land
)

type ReaperStore interface {
	ListOlderThan(ctx context.Context, status string, before time.Time, limit int) ([]store.Withdrawal, error)
	ListByStatus(ctx context.Context, status string, limit int) ([]store.Withdrawal, error)
	Transition(ctx context.Context, id, from, to string, reason *string) (store.Withdrawal, error)
	MarkSigned(ctx context.Context, id string, chainID int64, nonce uint64, rawTx string) (store.Withdrawal, error)
	StampBroadcastHead(ctx context.Context, id string, head int64) error
	ListTxs(ctx context.Context, id string) ([]store.Tx, error)
}

// Voider atomically asks wallet-service to never sign a Withdrawal Key (invariant #22; task 3.13).
// Until that endpoint exists the Reaper's Voider is nil and a stuck `reserved` row only raises an ALERT.
type Voider interface {
	VoidHotWallet(ctx context.Context, key string) (wallet.VoidResult, error)
}

// Reaper reclaims stuck Withdrawal Requests (withdrawal-flow.md invariant #17; task 3.9) — but only
// when it can PROVE nothing can still reach the chain and no Hot Wallet nonce is left allocated but
// unused. Every write is a guarded `UPDATE ... WHERE status = <expected>`, so it never overwrites a
// row the pipeline or request handler is moving at the same moment (ErrStateChanged is not an error).
//
//	requested  past StuckRequestTimeout      → failed (stale_request); nothing was ever held
//	validated  past StuckReservationTimeout  → Release, failed (stale_reservation); no nonce can exist
//	reserved   past StuckReservationTimeout  → void the key at wallet-service; voided → Release, failed;
//	                                           already signed → move on to `signed`; no Voider/error → ALERT
//	signed     past StuckReservationTimeout  → never Released on a timeout: ALERT naming the likely cause
//	signed / broadcast, nonce N consumed on-chain by another tx, no receipt for any of our hashes
//	           (broadcast: also StuckBroadcastBlocks old) → Release, failed (replaced_or_dropped)
//	broadcast  past StuckBroadcastBlocks, N unconsumed → ALERT only
type Reaper struct {
	Store  ReaperStore
	Ledger Ledger
	Chain  chain.Chain
	Voider Voider // nil until task 3.13
	// ChainID is recorded when a voided-too-late key turns out to be signed already.
	ChainID int64

	// Decided (Tech Lead): 5 minutes, 5 minutes, 128 blocks.
	StuckRequestTimeout     time.Duration
	StuckReservationTimeout time.Duration
	StuckBroadcastBlocks    int64

	// Recheck is the pre-sign whitelist/screening re-check (invariant #23); with it, a stuck `signed`
	// request that now fails it raises an ALERT (never blocks: the nonce is allocated). nil = off.
	Recheck func(ctx context.Context, w store.Withdrawal) (reason string, err error)

	Batch      int
	AlertEvery time.Duration // a given ALERT repeats at most this often; default 10 minutes
	Now        func() time.Time
	// Alert is where ALERTs go; default: the log, prefixed "ALERT:".
	Alert func(msg string)

	alerts alerter
}

// Run makes a pass every interval until ctx ends. A failed step is logged and retried.
func (r *Reaper) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Step(ctx); err != nil && ctx.Err() == nil {
			log.Printf("withdrawal reaper: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step makes one pass over every rule. Rules are independent: one failing does not stop the others.
func (r *Reaper) Step(ctx context.Context) error {
	return errors.Join(
		r.each(ctx, store.StatusRequested, r.StuckRequestTimeout, r.requested),
		r.each(ctx, store.StatusValidated, r.StuckReservationTimeout, r.validated),
		r.each(ctx, store.StatusReserved, r.StuckReservationTimeout, r.reserved),
		r.signed(ctx),
		r.broadcast(ctx),
	)
}

func (r *Reaper) each(ctx context.Context, status string, age time.Duration, f func(context.Context, store.Withdrawal) error) error {
	rows, err := r.Store.ListOlderThan(ctx, status, r.now().Add(-age), r.batch())
	if err != nil {
		return fmt.Errorf("listing stuck %s: %w", status, err)
	}
	return r.apply(ctx, status, rows, f)
}

func (r *Reaper) apply(ctx context.Context, status string, rows []store.Withdrawal, f func(context.Context, store.Withdrawal) error) error {
	var errs []error
	for _, w := range rows {
		if err := f(ctx, w); err != nil && !errors.Is(err, store.ErrStateChanged) {
			errs = append(errs, fmt.Errorf("%s %s: %w", status, w.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reaper) batch() int {
	if r.Batch > 0 {
		return r.Batch
	}
	// Stuck rows that are only ALERTed stay in the queue; this must stay well above how many there can be.
	return 500
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reaper) requested(ctx context.Context, w store.Withdrawal) error {
	reason := ReasonStaleRequest
	_, err := r.Store.Transition(ctx, w.ID, store.StatusRequested, store.StatusFailed, &reason)
	if err == nil {
		log.Printf("withdrawal %s: stuck in requested, failed (%s)", w.ID, reason)
	}
	return err
}

func (r *Reaper) validated(ctx context.Context, w store.Withdrawal) error {
	err := releaseAndFail(ctx, r.Store, r.Ledger, w, store.StatusValidated, ReasonStaleReservation)
	if err == nil {
		log.Printf("withdrawal %s: stuck in validated, Lock released, failed (%s)", w.ID, ReasonStaleReservation)
	}
	return err
}

func (r *Reaper) reserved(ctx context.Context, w store.Withdrawal) error {
	if r.Voider == nil {
		r.alert(w, "reserved-no-void", "stuck in reserved but wallet-service cannot void its key yet, so a signed nonce cannot be ruled out; not releasing")
		return nil
	}
	v, err := r.Voider.VoidHotWallet(ctx, w.Key())
	if err != nil {
		r.alert(w, "reserved-void-failed", fmt.Sprintf("stuck in reserved and wallet-service could not void its key: %v; not releasing, retrying next pass", err))
		return nil
	}
	if v.Voided {
		err := releaseAndFail(ctx, r.Store, r.Ledger, w, store.StatusReserved, ReasonStaleReservation)
		if err == nil {
			log.Printf("withdrawal %s: stuck in reserved, key voided, Lock released, failed (%s)", w.ID, ReasonStaleReservation)
		}
		return err
	}
	// A sign call had landed after all: from here on it is an ordinary signed request.
	if _, err := chain.TxHash(v.RawTx); err != nil {
		return fmt.Errorf("wallet-service returned an undecodable tx: %w", err)
	}
	_, err = r.Store.MarkSigned(ctx, w.ID, r.ChainID, v.Nonce, v.RawTx)
	return err
}

// signed: the on-chain proof releases it at any age; otherwise, past the timeout, an ALERT.
func (r *Reaper) signed(ctx context.Context) error {
	rows, err := r.Store.ListByStatus(ctx, store.StatusSigned, r.batch())
	if err != nil {
		return fmt.Errorf("listing signed: %w", err)
	}
	cutoff := r.now().Add(-r.StuckReservationTimeout)
	return r.apply(ctx, store.StatusSigned, rows, func(ctx context.Context, w store.Withdrawal) error {
		dead, mined, hot, err := r.nonceConsumedByOther(ctx, w)
		if err != nil {
			return err
		}
		if dead {
			return r.releaseDead(ctx, w, store.StatusSigned, mined)
		}
		if w.UpdatedAt.Before(cutoff) {
			r.alert(w, "signed-stuck", "stuck in signed, never released on a timeout: "+r.diagnose(ctx, w, hot, mined))
			r.alertIfNoLongerAllowed(ctx, w)
		}
		return nil
	})
}

// alertIfNoLongerAllowed: the request is signed, so it cannot be blocked without stranding its nonce;
// if the whitelist entry or Screening List now says no, an operator decides (let it go, or cancel it
// with a Nonce Filler).
func (r *Reaper) alertIfNoLongerAllowed(ctx context.Context, w store.Withdrawal) {
	if r.Recheck == nil {
		return
	}
	if reason, err := r.Recheck(ctx, w); err == nil && reason != "" {
		r.alert(w, "signed-recheck", fmt.Sprintf("signed but not broadcast, and it no longer passes the pre-sign re-check (%s): let it go out, or cancel it with a Nonce Filler (`service hot-wallet cancel --key %s`)", reason, w.Key()))
	}
}

// broadcast: acts only once StuckBroadcastBlocks have passed since the Reaper first saw it.
func (r *Reaper) broadcast(ctx context.Context) error {
	rows, err := r.Store.ListByStatus(ctx, store.StatusBroadcast, r.batch())
	if err != nil {
		return fmt.Errorf("listing broadcast: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	head, err := r.Chain.Head(ctx)
	if err != nil {
		return fmt.Errorf("reading the chain head: %w", err)
	}
	return r.apply(ctx, store.StatusBroadcast, rows, func(ctx context.Context, w store.Withdrawal) error {
		if w.BroadcastHeadSeen == nil {
			return r.Store.StampBroadcastHead(ctx, w.ID, head)
		}
		if head-*w.BroadcastHeadSeen < r.StuckBroadcastBlocks {
			return nil
		}
		dead, mined, hot, err := r.nonceConsumedByOther(ctx, w)
		if err != nil {
			return err
		}
		if dead {
			return r.releaseDead(ctx, w, store.StatusBroadcast, mined)
		}
		if mined > uint64(*w.Nonce) {
			return nil // consumed, and one of our hashes has a receipt: the pipeline confirms it
		}
		r.alert(w, "broadcast-stuck", fmt.Sprintf("broadcast %d blocks ago and nonce %d is still unconsumed: %s",
			head-*w.BroadcastHeadSeen, *w.Nonce, r.diagnose(ctx, w, hot, mined)))
		return nil
	})
}

// nonceConsumedByOther is the on-chain proof that nothing of ours can still land: the Hot Wallet's
// MINED nonce is past the request's nonce N, and no tx hash of this request has a receipt. The
// mined (`latest`) nonce, never `pending`, which counts our own tx still in a mempool and would
// "prove" a live tx dead. The nonce is read BEFORE the receipts: read the other way round, our own
// tx could be mined between the two reads and look like another tx's.
func (r *Reaper) nonceConsumedByOther(ctx context.Context, w store.Withdrawal) (dead bool, mined uint64, hot common.Address, err error) {
	if w.SignedRawTx == nil || w.Nonce == nil {
		return false, 0, hot, fmt.Errorf("%s request has no signed tx", w.Status)
	}
	if hot, err = chain.Sender(*w.SignedRawTx); err != nil {
		return false, 0, hot, err
	}
	if mined, err = r.Chain.MinedNonce(ctx, hot); err != nil {
		return false, 0, hot, err
	}
	if mined <= uint64(*w.Nonce) {
		return false, mined, hot, nil
	}
	hashes, err := txHashes(ctx, r.Store, w)
	if err != nil {
		return false, mined, hot, err
	}
	for _, h := range hashes {
		rc, err := r.Chain.Receipt(ctx, h)
		if err != nil {
			return false, mined, hot, err
		}
		if rc != nil {
			return false, mined, hot, nil
		}
	}
	return true, mined, hot, nil
}

// txHashes is every hash this request has ever had on the wire: the original, every Fee Bump
// replacement (invariant #21), and the one recorded as broadcast.
func txHashes(ctx context.Context, st interface {
	ListTxs(ctx context.Context, id string) ([]store.Tx, error)
}, w store.Withdrawal) ([]common.Hash, error) {
	h, err := chain.TxHash(*w.SignedRawTx)
	if err != nil {
		return nil, err
	}
	out := []common.Hash{h}
	txs, err := st.ListTxs(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range txs {
		out = append(out, common.HexToHash(t.Hash))
	}
	if w.TxHash != nil {
		known := false
		for _, o := range out {
			known = known || o == common.HexToHash(*w.TxHash)
		}
		if !known {
			out = append(out, common.HexToHash(*w.TxHash))
		}
	}
	return out, nil
}

func (r *Reaper) releaseDead(ctx context.Context, w store.Withdrawal, from string, mined uint64) error {
	err := releaseAndFail(ctx, r.Store, r.Ledger, w, from, ReasonReplacedOrDropped)
	if err == nil {
		log.Printf("withdrawal %s: nonce %d consumed by another tx (mined nonce %d), none of ours mined; Lock released, failed (%s)",
			w.ID, *w.Nonce, mined, ReasonReplacedOrDropped)
	}
	return err
}

// diagnose names the likely cause of a tx that is not getting mined, from live reads.
func (r *Reaper) diagnose(ctx context.Context, w store.Withdrawal, hot common.Address, mined uint64) string {
	var causes []string
	if mined < uint64(*w.Nonce) {
		causes = append(causes, fmt.Sprintf("an earlier Hot Wallet nonce is unmined (mined nonce %d < %d): this tx queues behind it", mined, *w.Nonce))
	}
	if b, err := hexutil.Decode(*w.SignedRawTx); err == nil {
		var tx types.Transaction
		if tx.UnmarshalBinary(b) == nil {
			if base, err := r.Chain.BaseFee(ctx); err == nil && tx.GasFeeCap().Cmp(base) < 0 {
				causes = append(causes, fmt.Sprintf("underpriced: max fee %s is below the base fee %s; needs a Fee Bump", tx.GasFeeCap(), base))
			}
			need := new(big.Int).Mul(tx.GasFeeCap(), new(big.Int).SetUint64(tx.Gas()))
			need.Add(need, tx.Value())
			if bal, err := r.Chain.Balance(ctx, hot); err == nil && bal.Cmp(need) < 0 {
				causes = append(causes, fmt.Sprintf("Hot Wallet %s lacks ETH: balance %s < %s needed; replenish Hot", hot.Hex(), bal, need))
			}
		}
	}
	if len(causes) == 0 {
		return fmt.Sprintf("cause not determined (nonce %d, mined nonce %d)", *w.Nonce, mined)
	}
	return fmt.Sprint(causes)
}

// alert reports a stuck request an operator must look at, at most once per AlertEvery per kind.
func (r *Reaper) alert(w store.Withdrawal, kind, msg string) {
	r.alerts.raise(r.now(), r.AlertEvery, r.Alert, w, kind, msg)
}
