package pipeline

import (
	"context"
	"strings"
	"testing"

	"digiwal/services/withdrawal-service/internal/store"
)

// disputeAfterReorg confirms key's request, reorgs its tx out and lets the detector dispute it.
func (r *rig) disputeConfirmed(key string) store.Withdrawal {
	r.t.Helper()
	r.reserved(key)
	r.confirmAll()
	r.reorgOut(key)
	_ = r.step()
	_ = r.step()
	w := r.get(key)
	if w.Status != store.StatusDisputed {
		r.t.Fatalf("%s is %s, want disputed", key, w.Status)
	}
	return w
}

func abandon(r *rig, id string) (store.Withdrawal, error) {
	return Abandon(context.Background(), r.st, r.ledger, r.chain, nil, id)
}

// Settled, then reorged out: once nonce N is provably consumed, a Compensating Entry reverses
// exactly the Settle's entries, under the deterministic refund key.
func TestAbandonASettledWithdrawalRefundsIt(t *testing.T) {
	r := newRig(t)
	w := r.disputeConfirmed("a")
	if _, err := abandon(r, w.ID); err == nil || !strings.Contains(err.Error(), "Nonce Filler") {
		t.Fatalf("abandon with nonce %d still open: %v, want a refusal", *w.Nonce, err)
	}
	if len(r.ledger.refunds) != 0 || r.get("a").Status != store.StatusDisputed {
		t.Fatal("refused, yet something moved")
	}
	r.chain.minedNonce = uint64(*w.Nonce) + 1 // an operator's Nonce Filler consumed nonce N
	got, err := abandon(r, w.ID)
	if err != nil || got.Status != store.StatusFailed || *got.RejectReason != ReasonAbandonedRefunded {
		t.Fatalf("%+v %v", got, err)
	}
	key := "withdrawal:" + w.ID
	if e := r.ledger.refunds[key]; len(e) != 2 || e[0] != "e1-"+key || e[1] != "e2-"+key {
		t.Fatalf("refund corrected %v, want the Settle's two entries", e)
	}
	if len(r.ledger.released) != 0 {
		t.Fatal("a Settled withdrawal must be refunded, not released")
	}
	if _, err := abandon(r, w.ID); err == nil {
		t.Fatal("abandoned twice")
	}
}

// Never Settled (broadcast, disputed by a gap below it): the Lock is Released, nothing refunded.
func TestAbandonAnUnsettledWithdrawalReleasesIt(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.confirmAll() // a = nonce 0, confirmed
	r.reserved("b")
	_ = r.step() // b = nonce 1, broadcast
	r.reorgOut("a")
	_ = r.step()
	_ = r.step()
	b := r.get("b")
	if b.Status != store.StatusDisputed || *b.DisputedFrom != store.StatusBroadcast {
		t.Fatalf("b = %s from %v", b.Status, b.DisputedFrom)
	}
	r.chain.minedNonce = 2
	got, err := abandon(r, b.ID)
	if err != nil || got.Status != store.StatusFailed || *got.RejectReason != ReasonAbandoned {
		t.Fatalf("%+v %v", got, err)
	}
	if len(r.ledger.released) != 1 || r.ledger.released[0] != "withdrawal:"+b.ID || len(r.ledger.refunds) != 0 {
		t.Fatalf("released %v refunds %v", r.ledger.released, r.ledger.refunds)
	}
}

// A tx that is mined paid out: abandoning it would hand the user the money twice.
func TestAbandonRefusesAMinedTx(t *testing.T) {
	r := newRig(t)
	w := r.disputeConfirmed("a")
	r.chain.mine(true) // it was re-included after all
	r.chain.minedNonce = uint64(*w.Nonce) + 1
	if _, err := abandon(r, w.ID); err == nil || !strings.Contains(err.Error(), "is mined") {
		t.Fatalf("%v, want a refusal", err)
	}
	if len(r.ledger.refunds) != 0 || r.get("a").Status != store.StatusDisputed {
		t.Fatal("moved a request whose tx is mined")
	}
}

func TestAbandonOnlyDisputedRequests(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.confirmAll()
	r.chain.minedNonce = 10
	if _, err := abandon(r, r.get("a").ID); err == nil {
		t.Fatal("abandoned a confirmed request")
	}
}
