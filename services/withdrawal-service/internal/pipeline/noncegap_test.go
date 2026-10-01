package pipeline

import (
	"context"
	"errors"
	"testing"

	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/store"
)

// confirmAll walks every reserved request to `confirmed` (nonces 0, 1, ... in key order).
func (r *rig) confirmAll() {
	r.t.Helper()
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	if err := r.step(); err != nil {
		r.t.Fatal(err)
	}
}

// reorgOut drops the receipt of key's tx, as a reorg deeper than the confirmation depth would.
func (r *rig) reorgOut(key string) {
	r.t.Helper()
	h, _ := chain.TxHash(*r.get(key).SignedRawTx)
	delete(r.chain.receipts, h)
}

func TestNonceGapDisputesEverythingFromTheGapOn(t *testing.T) {
	r := newRig(t)
	for _, k := range []string{"a", "b", "c"} {
		r.reserved(k)
	}
	r.confirmAll() // a=0, b=1, c=2 confirmed
	r.reserved("d")
	_ = r.step() // d=3 broadcast, not mined
	r.reserved("e")
	r.signer.failNext = 100 // e stays reserved: no nonce yet

	r.reorgOut("b")
	_ = r.step()
	if r.get("b").Status != store.StatusConfirmed || len(r.rep.calls) != 0 {
		t.Fatal("a single missing receipt already declared a gap")
	}
	_ = r.step()
	for k, want := range map[string]string{"a": "confirmed", "b": "disputed", "c": "disputed", "d": "disputed", "e": "reserved"} {
		if got := r.get(k); got.Status != want {
			t.Errorf("%s: %s, want %s", k, got.Status, want)
		}
	}
	if from := r.get("b").DisputedFrom; from == nil || *from != "confirmed" {
		t.Errorf("b disputed_from = %v", from)
	}
	if from := r.get("d").DisputedFrom; from == nil || *from != "broadcast" {
		t.Errorf("d disputed_from = %v", from)
	}
	if len(r.rep.calls) != 1 || r.rep.calls[0] != "31337/1" {
		t.Fatalf("reported %v, want one report at nonce 1", r.rep.calls)
	}
	// Disputed requests are the operator's: the pipeline no longer settles or touches them.
	settled := len(r.ledger.settled)
	r.chain.mine(true)
	r.chain.head += depth
	_ = r.step()
	_ = r.step()
	if len(r.ledger.settled) != settled || r.get("d").Status != store.StatusDisputed || len(r.rep.calls) != 1 {
		t.Fatalf("pipeline touched disputed requests or re-reported (settled %d->%d, reports %v)", settled, len(r.ledger.settled), r.rep.calls)
	}
}

func TestNonceGapReportIsRetriedUntilAcknowledged(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.confirmAll()
	r.reorgOut("a")
	r.rep.err = errors.New("wallet-service down")
	_ = r.step()
	_ = r.step() // gap declared and disputed, report fails
	if r.get("a").Status != store.StatusDisputed {
		t.Fatal("not disputed while the report failed — disputes must come first")
	}
	_ = r.step()
	r.rep.err = nil
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	gaps, _ := r.st.UnreportedNonceGaps(context.Background())
	if len(gaps) != 0 || r.rep.calls[len(r.rep.calls)-1] != "31337/0" {
		t.Fatalf("unreported %v, calls %v", gaps, r.rep.calls)
	}
	n := len(r.rep.calls)
	_ = r.step()
	if len(r.rep.calls) != n {
		t.Fatal("reported again after acknowledgement")
	}
}

// One flaky RPC answer (receipt briefly missing) must not freeze all Hot Wallet signing.
func TestAFlakyReceiptIsNotAGap(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.confirmAll()
	h, _ := chain.TxHash(*r.get("a").SignedRawTx)
	saved := r.chain.receipts[h]
	delete(r.chain.receipts, h)
	_ = r.step()
	r.chain.receipts[h] = saved
	_ = r.step()
	delete(r.chain.receipts, h)
	_ = r.step()
	if r.get("a").Status != store.StatusConfirmed || len(r.rep.calls) != 0 {
		t.Fatal("non-consecutive misses declared a gap")
	}
}

// Outside the window, a confirmed tx is no longer re-checked.
func TestOldConfirmationsAreNotRechecked(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.confirmAll()
	r.chain.head += 1000
	r.reorgOut("a")
	_ = r.step()
	_ = r.step()
	if r.get("a").Status != store.StatusConfirmed || len(r.rep.calls) != 0 {
		t.Fatal("re-checked a confirmation older than the window")
	}
}
