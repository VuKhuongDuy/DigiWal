package pipeline

import (
	"context"
	"fmt"
	"log"
	"time"

	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/store"
)

// Failure reasons for an operator-abandoned disputed request.
const (
	ReasonAbandoned         = "abandoned"          // never Settled: its Lock was Released
	ReasonAbandonedRefunded = "abandoned_refunded" // had Settled: a Compensating Entry gave it back
)

type AbandonStore interface {
	GetByID(ctx context.Context, id string) (store.Withdrawal, error)
	ListTxs(ctx context.Context, id string) ([]store.Tx, error)
	MarkFailed(ctx context.Context, id, from, reason, blockAddress string, gas *store.GasRecord, event func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error)
}

type AbandonLedger interface {
	Ledger
	Refund(ctx context.Context, key string, settleEntryIDs []string) error
}

// Abandon resolves one `disputed` request whose tx can never land (withdrawal-flow.md invariant #9;
// task 3.5). It is an explicit operator action, never automatic, and it refuses unless the chain
// proves the tx is dead: the tx is not mined, and the Hot Wallet's mined (`latest`) nonce is past
// the request's nonce — i.e. another tx (normally an operator's Nonce Filler) consumed it. Then:
//   - it had Settled (`disputed_from = confirmed`): the real debit is reversed by a Compensating
//     Entry — a correction of exactly the Settle's entries, keyed {Withdrawal Key}:refund, amounts
//     derived by the ledger, never asserted here;
//   - it had not: its Lock is simply Released (no Entry was ever posted).
//
// Then `disputed → failed`. Every step is idempotent, so a crash halfway is fixed by running it again.
func Abandon(ctx context.Context, st AbandonStore, lg AbandonLedger, ch chain.Chain, fillers Fillers, id string) (store.Withdrawal, error) {
	w, err := st.GetByID(ctx, id)
	if err != nil {
		return store.Withdrawal{}, err
	}
	if w.Status != store.StatusDisputed || w.DisputedFrom == nil || w.SignedRawTx == nil || w.Nonce == nil {
		return store.Withdrawal{}, fmt.Errorf("withdrawal %s is %s, not a disputed request with a tx", id, w.Status)
	}
	hash, err := chain.TxHash(*w.SignedRawTx)
	if err != nil {
		return store.Withdrawal{}, err
	}
	hashes, err := txHashes(ctx, st, w) // the original and every Fee Bump: any of them may be the one that landed
	if err != nil {
		return store.Withdrawal{}, err
	}
	for _, h := range hashes {
		if r, err := ch.Receipt(ctx, h); err != nil {
			return store.Withdrawal{}, err
		} else if r != nil {
			return store.Withdrawal{}, fmt.Errorf("refusing: tx %s is mined (block %d) — it paid out; restore the request instead of abandoning it", h.Hex(), r.BlockNumber)
		}
	}
	hot, err := chain.Sender(*w.SignedRawTx)
	if err != nil {
		return store.Withdrawal{}, err
	}
	mined, err := ch.MinedNonce(ctx, hot)
	if err != nil {
		return store.Withdrawal{}, err
	}
	if mined <= uint64(*w.Nonce) {
		return store.Withdrawal{}, fmt.Errorf("refusing: Hot Wallet %s has mined nonces up to %d, nonce %d is still open — the original tx could still land; send a Nonce Filler at %d first", hot.Hex(), mined, *w.Nonce, *w.Nonce)
	}

	reason := ReasonAbandoned
	if *w.DisputedFrom == store.StatusConfirmed {
		settled, err := lg.Settle(ctx, w.Key()) // a replay: returns the Settle's entries
		if err != nil {
			return store.Withdrawal{}, fmt.Errorf("reading the Settle to refund: %w", err)
		}
		if err := lg.Refund(ctx, w.Key(), settled.EntryIDs); err != nil {
			return store.Withdrawal{}, fmt.Errorf("refunding: %w", err)
		}
		reason = ReasonAbandonedRefunded
	} else if err := lg.Release(ctx, w.Key()); err != nil {
		return store.Withdrawal{}, fmt.Errorf("releasing: %w", err)
	}
	gas, err := fillerGas(ctx, fillers, ch, w.Key()) // the filler that consumed the nonce paid gas too (task 3.15)
	if err != nil {
		return store.Withdrawal{}, fmt.Errorf("looking for a mined Nonce Filler: %w", err)
	}
	if err := failWith(ctx, st, w, store.StatusDisputed, failure{reason: reason, gas: gas}, 0, time.Now()); err != nil {
		return store.Withdrawal{}, err
	}
	out, err := st.GetByID(ctx, w.ID)
	if err != nil {
		return store.Withdrawal{}, err
	}
	log.Printf("withdrawal %s abandoned (%s): tx %s at nonce %d never landed", w.ID, reason, hash.Hex(), *w.Nonce)
	return out, nil
}
