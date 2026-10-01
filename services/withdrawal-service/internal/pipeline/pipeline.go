// Package pipeline moves a reserved Withdrawal Request the rest of the way (withdrawal-flow.md
// invariants #1-#6, #16, #20; task 3.3): sign → broadcast → wait for Withdrawal Confirmation Depth →
// Settle, publishing withdrawals.broadcast / withdrawals.confirmed through the outbox.
//
// Every step is safe to repeat, so a crash anywhere just means the next poll redoes it:
//   - signing is keyed on the Withdrawal Key, and wallet-service returns the stored tx for a key it
//     already signed (#16) — a retry never allocates a second nonce;
//   - the signed raw tx is stored before broadcast, and only that exact tx is ever (re)sent;
//   - Settle and Release are idempotent on the ledger side;
//   - every state change is a guarded UPDATE ... WHERE status = <expected> (#11).
//
// It only ever moves a request FORWARD once wallet-service may have allocated a nonce for it: a
// signed tx that is dropped instead of broadcast would strand that Hot Wallet nonce and block
// every later Hot Wallet tx (the failure #16 exists to prevent).
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/pkg/events"
	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

// Failure reasons recorded on a `failed` row (reject_reason column).
const (
	ReasonSignRefused = "sign_refused" // wallet-service refused the request; no nonce allocated
	ReasonTxReverted  = "tx_reverted"  // mined, but reverted: nothing was paid out
)

type Store interface {
	ListByStatus(ctx context.Context, status string, limit int) ([]store.Withdrawal, error)
	Transition(ctx context.Context, id, from, to string, reason *string) (store.Withdrawal, error)
	MarkSigned(ctx context.Context, id string, chainID int64, nonce uint64, rawTx string) (store.Withdrawal, error)
	MarkBroadcast(ctx context.Context, id, txHash string, at time.Time, event func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error)
	MarkConfirmed(ctx context.Context, id string, block int64, ledgerTxID string, at time.Time, event func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error)
	UnpublishedEvents(ctx context.Context, limit int) ([]store.OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error
}

type Ledger interface {
	Settle(ctx context.Context, key string) (string, error)
	Release(ctx context.Context, key string) error
}

type Signer interface {
	SignHotWallet(ctx context.Context, r wallet.SignRequest) (rawTx string, nonce uint64, err error)
}

type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

type Pipeline struct {
	Store     Store
	Ledger    Ledger
	Signer    Signer
	Chain     chain.Chain
	Publisher Publisher
	ChainID   int64
	// ConfirmationDepth: a tx mined in block B is final once head >= B + depth (Decided: 64 blocks;
	// configurable so a local chain needs fewer).
	ConfirmationDepth int64
	Batch             int
	Now               func() time.Time
	// Gaps enables Nonce Gap detection (task 3.4); nil disables it.
	Gaps *GapWatch
}

// Run polls until ctx ends. A failed step is logged and retried on the next tick.
func (p *Pipeline) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := p.Step(ctx); err != nil && ctx.Err() == nil {
			log.Printf("withdrawal pipeline: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step makes one pass over every stage. Stages are independent: one failing does not stop the
// others, and a request can advance several stages in one pass.
func (p *Pipeline) Step(ctx context.Context) error {
	return errors.Join(
		p.each(ctx, store.StatusReserved, p.sign),
		p.each(ctx, store.StatusSigned, p.broadcast),
		p.each(ctx, store.StatusBroadcast, p.confirm),
		p.publish(ctx),
		p.watchGaps(ctx),
	)
}

func (p *Pipeline) each(ctx context.Context, status string, f func(context.Context, store.Withdrawal) error) error {
	rows, err := p.Store.ListByStatus(ctx, status, p.batch())
	if err != nil {
		return fmt.Errorf("listing %s: %w", status, err)
	}
	var errs []error
	for _, w := range rows {
		if err := f(ctx, w); err != nil && !errors.Is(err, store.ErrStateChanged) {
			errs = append(errs, fmt.Errorf("%s %s: %w", status, w.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Pipeline) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return 50
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// sign: reserved → signed. A refusal (400/422: no nonce allocated) Releases the Lock and fails the
// request; a 409 (the key is already signed for a different request) is an ALERT and is never
// released, since a nonce exists under that key.
// Anything else — wallet-service down, a Nonce Gap freeze (503) — is retried with the same key.
func (p *Pipeline) sign(ctx context.Context, w store.Withdrawal) error {
	raw, nonce, err := p.Signer.SignHotWallet(ctx, wallet.SignRequest{
		ToAddress: w.ToAddress, AssetID: w.AssetID, Amount: w.Amount.String(), ChainID: p.ChainID, IdempotencyKey: w.Key(),
	})
	if errors.Is(err, wallet.ErrSignRefused) {
		log.Printf("withdrawal %s: %v — releasing its Lock", w.ID, err)
		return p.releaseAndFail(ctx, w, store.StatusReserved, ReasonSignRefused)
	}
	if errors.Is(err, wallet.ErrKeyConflict) {
		log.Printf("ALERT: withdrawal %s: %v — a nonce may be allocated under its key; not releasing", w.ID, err)
		return err
	}
	if err != nil {
		return err
	}
	if _, err := chain.TxHash(raw); err != nil {
		return fmt.Errorf("wallet-service returned an undecodable tx: %w", err)
	}
	_, err = p.Store.MarkSigned(ctx, w.ID, p.ChainID, nonce, raw)
	return err
}

// broadcast: signed → broadcast. Sends the stored raw tx (never a new one). A node that already has
// it, or a tx that is already mined, counts as sent.
func (p *Pipeline) broadcast(ctx context.Context, w store.Withdrawal) error {
	hash, err := chain.TxHash(*w.SignedRawTx)
	if err != nil {
		return err
	}
	if err := p.Chain.Broadcast(ctx, *w.SignedRawTx); err != nil {
		r, rerr := p.Chain.Receipt(ctx, hash)
		if rerr != nil || r == nil {
			if strings.Contains(strings.ToLower(err.Error()), "nonce too low") {
				// Our nonce was used by a different tx: this one can never land. Not ours to
				// resolve automatically (invariant #4): an operator decides.
				log.Printf("ALERT: withdrawal %s: tx %s rejected, nonce %d already used by another tx: %v", w.ID, hash.Hex(), *w.Nonce, err)
			}
			return fmt.Errorf("broadcasting %s: %w", hash.Hex(), err)
		}
	}
	at := p.now().UTC()
	_, err = p.Store.MarkBroadcast(ctx, w.ID, strings.ToLower(hash.Hex()), at, func(w store.Withdrawal) (string, []byte, error) {
		b, err := events.Encode(events.WithdrawalBroadcast{
			Version: 1, WithdrawalID: w.ID, IdempotencyKey: w.IdempotencyKey, UserID: w.UserID, AssetID: w.AssetID,
			Amount: events.Amount(w.Amount.String()), ToAddress: w.ToAddress, ChainID: *w.ChainID,
			TxHash: *w.TxHash, Nonce: *w.Nonce, BroadcastAt: at,
		})
		return events.TopicWithdrawalsBroadcast, b, err
	})
	return err
}

// confirm: broadcast → confirmed, once the tx is Withdrawal Confirmation Depth deep. The Settle comes
// first — it is the only point where the debit Entry is posted (#3) — then the state change.
// A tx that was mined but reverted paid nothing out: its Lock is Released and the request fails.
func (p *Pipeline) confirm(ctx context.Context, w store.Withdrawal) error {
	r, err := p.Chain.Receipt(ctx, common.HexToHash(*w.TxHash))
	if err != nil || r == nil {
		return err // not mined yet (nil, nil), or an RPC error
	}
	head, err := p.Chain.Head(ctx)
	if err != nil {
		return err
	}
	if head < r.BlockNumber+p.ConfirmationDepth {
		return nil
	}
	if !r.Success {
		// Decided (Tech Lead): only once the revert is itself Withdrawal Confirmation Depth deep (a
		// shallower receipt can still change under a reorg); Release, fail, no auto-retry. The revert
		// reason is recorded; a Hot Wallet short of the asset is our fault, not the user's: ALERT.
		reason, rerr := p.Chain.RevertReason(ctx, common.HexToHash(*w.TxHash), r.BlockNumber)
		if rerr != nil {
			reason = "unknown (" + rerr.Error() + ")"
		}
		level := "withdrawal"
		if HotWalletShort(reason) {
			level = "ALERT: withdrawal"
		}
		log.Printf("%s %s: tx %s reverted in block %d, revert reason %q — releasing its Lock", level, w.ID, *w.TxHash, r.BlockNumber, reason)
		return p.releaseAndFail(ctx, w, store.StatusBroadcast, ReasonTxReverted)
	}
	ledgerTx, err := p.Ledger.Settle(ctx, w.Key())
	if errors.Is(err, ledger.ErrConflict) || errors.Is(err, ledger.ErrLockNotFound) {
		// The Lock was released (or never existed) although the tx paid out: the user got the money
		// and kept the balance. Never paper over it automatically.
		log.Printf("ALERT: withdrawal %s: tx %s confirmed but Settle failed: %v — needs an operator", w.ID, *w.TxHash, err)
		return err
	}
	if err != nil {
		return err
	}
	at := p.now().UTC()
	_, err = p.Store.MarkConfirmed(ctx, w.ID, r.BlockNumber, ledgerTx, at, func(w store.Withdrawal) (string, []byte, error) {
		b, err := events.Encode(events.WithdrawalConfirmed{
			Version: 1, WithdrawalID: w.ID, IdempotencyKey: w.IdempotencyKey, UserID: w.UserID, AssetID: w.AssetID,
			Amount: events.Amount(w.Amount.String()), ToAddress: w.ToAddress, ChainID: *w.ChainID,
			TxHash: *w.TxHash, Nonce: *w.Nonce, BlockNumber: head, ConfirmedAt: at,
		})
		return events.TopicWithdrawalsConfirmed, b, err
	})
	return err
}

// HotWalletShort reports whether a revert message says the sender lacked funds — for a withdrawal
// that is the Hot Wallet running short of the asset, an operational fault that ops must fix.
func HotWalletShort(revertReason string) bool {
	m := strings.ToLower(revertReason)
	for _, s := range []string{"insufficient balance", "exceeds balance", "insufficient funds", "transfer amount exceeds"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// releaseAndFail gives the Lock back, then marks the request failed. Release first: if the
// transition then fails, the next pass Releases again (idempotent) and retries the transition.
func (p *Pipeline) releaseAndFail(ctx context.Context, w store.Withdrawal, from, reason string) error {
	if err := p.Ledger.Release(ctx, w.Key()); err != nil {
		return fmt.Errorf("releasing: %w", err)
	}
	_, err := p.Store.Transition(ctx, w.ID, from, store.StatusFailed, &reason)
	return err
}

// publish sends outbox events Kafka has not acknowledged yet, in order, and marks each published
// only after the acknowledgement. Stops at the first failure so order is kept.
func (p *Pipeline) publish(ctx context.Context) error {
	evs, err := p.Store.UnpublishedEvents(ctx, p.batch())
	if err != nil {
		return fmt.Errorf("reading outbox: %w", err)
	}
	for _, e := range evs {
		if err := p.Publisher.Publish(ctx, e.Topic, e.WithdrawalID, e.Payload); err != nil {
			return err
		}
		if err := p.Store.MarkPublished(ctx, e.ID); err != nil {
			return err
		}
	}
	return nil
}
