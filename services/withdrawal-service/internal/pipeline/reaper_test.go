package pipeline

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

type fakeVoider struct {
	res   VoidResult
	err   error
	calls []string
}

func (v *fakeVoider) VoidHotWallet(_ context.Context, key string) (VoidResult, error) {
	v.calls = append(v.calls, key)
	return v.res, v.err
}

// reaper returns a Reaper whose clock is `age` ahead of real time, so rows created now are `age` old.
func (r *rig) reaper(age time.Duration) (*Reaper, *[]string) {
	var alerts []string
	return &Reaper{Store: r.st, Ledger: r.ledger, Chain: r.chain, ChainID: chainID,
		StuckRequestTimeout: 5 * time.Minute, StuckReservationTimeout: 5 * time.Minute, StuckBroadcastBlocks: 128,
		Now: func() time.Time { return time.Now().Add(age) }, Alert: func(m string) { alerts = append(alerts, m) }}, &alerts
}

const stuck = 6 * time.Minute

func (r *rig) newRequest(key string) store.Withdrawal {
	r.t.Helper()
	w, _, err := r.st.Create(context.Background(), store.NewRequest{UserID: "alice", IdempotencyKey: key,
		AssetID: "11111111-1111-1111-1111-111111111111", Amount: big.NewInt(1_000_000_000_000_000), ToAddress: toAddr})
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) validated(key string) store.Withdrawal {
	r.t.Helper()
	w := r.newRequest(key)
	w, err := r.st.Transition(context.Background(), w.ID, store.StatusRequested, store.StatusValidated, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// signedOnly walks a request to `signed` and stops: signed, never broadcast.
func (r *rig) signedOnly(key string) store.Withdrawal {
	r.t.Helper()
	w := r.reserved(key)
	if err := r.p.sign(context.Background(), w); err != nil {
		r.t.Fatal(err)
	}
	if got := r.get(key); got.Status != store.StatusSigned {
		r.t.Fatalf("%s is %s, want signed", key, got.Status)
	}
	return r.get(key)
}

func (r *rig) broadcastOnly(key string) store.Withdrawal {
	r.t.Helper()
	r.reserved(key)
	_ = r.step()
	if got := r.get(key); got.Status != store.StatusBroadcast {
		r.t.Fatalf("%s is %s, want broadcast", key, got.Status)
	}
	return r.get(key)
}

func (r *rig) reap(rp *Reaper) {
	r.t.Helper()
	if err := rp.Step(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func TestReaperFailsAStuckRequestedRowWithoutTouchingTheLedger(t *testing.T) {
	r := newRig(t)
	r.newRequest("old")
	fresh, _ := r.reaper(0)
	r.reap(fresh)
	if r.get("old").Status != store.StatusRequested {
		t.Fatal("a fresh requested row was reaped")
	}
	rp, _ := r.reaper(stuck)
	r.reap(rp)
	got := r.get("old")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonStaleRequest {
		t.Fatalf("%+v", got)
	}
	if len(r.ledger.released) != 0 || len(r.ledger.settled) != 0 {
		t.Fatalf("ledger touched for a requested row: released %v", r.ledger.released)
	}
}

func TestReaperReleasesAStuckValidatedRow(t *testing.T) {
	r := newRig(t)
	w := r.validated("k")
	fresh, _ := r.reaper(0)
	r.reap(fresh)
	if r.get("k").Status != store.StatusValidated || len(r.ledger.released) != 0 {
		t.Fatal("a fresh validated row was reaped")
	}
	rp, _ := r.reaper(stuck)
	r.reap(rp)
	got := r.get("k")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonStaleReservation ||
		len(r.ledger.released) != 1 || r.ledger.released[0] != w.Key() {
		t.Fatalf("%+v released %v", got, r.ledger.released)
	}
}

// Until wallet-service can void a key, a stuck `reserved` row may have a signed nonce: only an ALERT.
func TestReaperNeverReleasesReservedWithoutAVoider(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	rp, alerts := r.reaper(stuck)
	r.reap(rp)
	r.reap(rp)
	if r.get("k").Status != store.StatusReserved || len(r.ledger.released) != 0 {
		t.Fatal("released a reserved row with no way to rule out a signed nonce")
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "cannot void") {
		t.Fatalf("alerts = %q, want exactly one (throttled)", *alerts)
	}
}

func TestReaperReservedFollowsWalletServicesVoid(t *testing.T) {
	t.Run("voided → Release and fail", func(t *testing.T) {
		r := newRig(t)
		w := r.reserved("k")
		rp, _ := r.reaper(stuck)
		v := &fakeVoider{res: VoidResult{Voided: true}}
		rp.Voider = v
		r.reap(rp)
		got := r.get("k")
		if got.Status != store.StatusFailed || *got.RejectReason != ReasonStaleReservation ||
			len(v.calls) != 1 || v.calls[0] != w.Key() || len(r.ledger.released) != 1 {
			t.Fatalf("%+v voids %v released %v", got, v.calls, r.ledger.released)
		}
	})
	t.Run("already signed → becomes signed, nothing released", func(t *testing.T) {
		r := newRig(t)
		w := r.reserved("k")
		raw, nonce, _ := r.signer.SignHotWallet(context.Background(), signReq(w))
		rp, _ := r.reaper(stuck)
		rp.Voider = &fakeVoider{res: VoidResult{RawTx: raw, Nonce: nonce}}
		r.reap(rp)
		got := r.get("k")
		if got.Status != store.StatusSigned || *got.SignedRawTx != raw || *got.Nonce != int64(nonce) || *got.ChainID != chainID || len(r.ledger.released) != 0 {
			t.Fatalf("%+v released %v", got, r.ledger.released)
		}
	})
	t.Run("wallet-service down → ALERT, untouched", func(t *testing.T) {
		r := newRig(t)
		r.reserved("k")
		rp, alerts := r.reaper(stuck)
		rp.Voider = &fakeVoider{err: errors.New("503 frozen")}
		r.reap(rp)
		if r.get("k").Status != store.StatusReserved || len(r.ledger.released) != 0 || len(*alerts) != 1 {
			t.Fatalf("status %s released %v alerts %q", r.get("k").Status, r.ledger.released, *alerts)
		}
	})
}

// A signed row is NEVER released on time alone — only on proof that its nonce is consumed.
func TestReaperNeverReleasesSignedOnATimeout(t *testing.T) {
	r := newRig(t)
	r.signedOnly("k")
	r.chain.baseFee = 1_000 // above the fake's max fee of 2: underpriced
	rp, alerts := r.reaper(24 * time.Hour)
	r.reap(rp)
	if r.get("k").Status != store.StatusSigned || len(r.ledger.released) != 0 {
		t.Fatal("released a signed row on a timeout")
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "underpriced") {
		t.Fatalf("alerts = %q, want one naming underpriced", *alerts)
	}
}

func TestReaperSignedAlertNamesAnEmptyHotWallet(t *testing.T) {
	r := newRig(t)
	r.signedOnly("k")
	r.chain.balance = big.NewInt(1)
	rp, alerts := r.reaper(stuck)
	r.reap(rp)
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "lacks ETH") {
		t.Fatalf("alerts = %q", *alerts)
	}
}

func TestReaperSignedAlertNamesAnEarlierUnminedNonce(t *testing.T) {
	r := newRig(t)
	r.reserved("a")
	r.reserved("b")
	_ = r.p.each(context.Background(), store.StatusReserved, r.p.sign)
	// "b" has nonce 1; nothing is mined (mined nonce 0) → it queues behind nonce 0.
	rp, alerts := r.reaper(stuck)
	r.reap(rp)
	joined := strings.Join(*alerts, "\n")
	if !strings.Contains(joined, "queues behind") {
		t.Fatalf("alerts = %q", *alerts)
	}
}

func TestReaperSignedIsReleasedOnceItsNonceIsConsumedByAnotherTx(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k") // nonce 0

	// Nonce 0 still open: not released, even though it is old.
	rp, _ := r.reaper(stuck)
	r.reap(rp)
	if r.get("k").Status != store.StatusSigned || len(r.ledger.released) != 0 {
		t.Fatal("released with its nonce still open")
	}

	// Nonce 0 mined — by someone else (an operator's Nonce Filler), no receipt for our hash. This
	// applies at ANY age: a fresh signed row is released too.
	r.chain.minedNonce = 1
	fresh, _ := r.reaper(0)
	r.reap(fresh)
	got := r.get("k")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonReplacedOrDropped ||
		len(r.ledger.released) != 1 || r.ledger.released[0] != w.Key() {
		t.Fatalf("%+v released %v", got, r.ledger.released)
	}
}

// The nonce is consumed, but BY OUR OWN tx (it has a receipt): that is not proof of anything dead.
func TestReaperDoesNotReleaseWhenOurOwnTxIsMined(t *testing.T) {
	r := newRig(t)
	r.broadcastOnly("k")
	r.chain.mine(true)
	r.chain.minedNonce = 1
	r.chain.head += 1_000
	rp, alerts := r.reaper(stuck)
	r.reap(rp) // stamps
	r.chain.head += 1_000
	r.reap(rp)
	if r.get("k").Status != store.StatusBroadcast || len(r.ledger.released) != 0 || len(*alerts) != 0 {
		t.Fatalf("status %s released %v alerts %q", r.get("k").Status, r.ledger.released, *alerts)
	}
}

func TestReaperBroadcastWaits128BlocksThenNeedsProof(t *testing.T) {
	r := newRig(t)
	r.broadcastOnly("k") // nonce 0, head 100
	rp, alerts := r.reaper(0)

	r.reap(rp) // first sight: stamps head 100, acts on nothing
	seen := r.get("k").BroadcastHeadSeen
	if seen == nil || *seen != 100 {
		t.Fatalf("broadcast_head_seen = %v", seen)
	}

	// Nonce consumed by another tx, but only 127 blocks have passed: not yet.
	r.chain.minedNonce = 1
	r.chain.head = 100 + 127
	r.reap(rp)
	if r.get("k").Status != store.StatusBroadcast || len(r.ledger.released) != 0 {
		t.Fatal("released before the Stuck Broadcast Timeout")
	}

	r.chain.head = 100 + 128
	r.reap(rp)
	got := r.get("k")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonReplacedOrDropped || len(r.ledger.released) != 1 {
		t.Fatalf("%+v released %v", got, r.ledger.released)
	}
	if len(*alerts) != 0 {
		t.Fatalf("alerts = %q", *alerts)
	}
}

func TestReaperBroadcastWithTheNonceStillOpenOnlyAlerts(t *testing.T) {
	r := newRig(t)
	r.broadcastOnly("k")
	r.chain.baseFee = 1_000
	rp, alerts := r.reaper(0)
	r.reap(rp)
	r.chain.head = 100 + 500
	r.reap(rp)
	r.reap(rp)
	if r.get("k").Status != store.StatusBroadcast || len(r.ledger.released) != 0 {
		t.Fatal("released a broadcast row whose nonce is still unconsumed")
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "still unconsumed") || !strings.Contains((*alerts)[0], "underpriced") {
		t.Fatalf("alerts = %q", *alerts)
	}
}

// The Reaper never overwrites a row someone else just moved: the guarded UPDATE reports it, the
// pass is not an error, and the new status stands.
type racing struct {
	*store.Store
	t    *testing.T
	flip func(store.Withdrawal)
}

func (s racing) ListOlderThan(ctx context.Context, status string, before time.Time, limit int) ([]store.Withdrawal, error) {
	rows, err := s.Store.ListOlderThan(ctx, status, before, limit)
	if status == store.StatusRequested {
		for _, w := range rows {
			s.flip(w) // the request handler moves it after the Reaper has read it
		}
	}
	return rows, err
}

func TestReaperLosesTheRaceGracefully(t *testing.T) {
	r := newRig(t)
	w := r.newRequest("k")
	rp, _ := r.reaper(stuck)
	rp.StuckReservationTimeout = 24 * time.Hour // the skewed test clock would otherwise age the flipped row too
	rp.Store = racing{Store: r.st, t: t, flip: func(w store.Withdrawal) {
		if _, err := r.st.Transition(context.Background(), w.ID, store.StatusRequested, store.StatusValidated, nil); err != nil {
			t.Fatal(err)
		}
	}}
	if err := rp.Step(context.Background()); err != nil {
		t.Fatalf("losing the race is not an error: %v", err)
	}
	if got := r.get("k"); got.Status != store.StatusValidated || got.ID != w.ID {
		t.Fatalf("the Reaper overwrote a row that had moved: %s", got.Status)
	}
	if len(r.ledger.released) != 0 {
		t.Fatalf("released %v", r.ledger.released)
	}
}

func TestReaperLeavesSettledAndTerminalRowsAlone(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	_ = r.step()
	if r.get("k").Status != store.StatusConfirmed {
		t.Fatal("setup: not confirmed")
	}
	rp, alerts := r.reaper(24 * time.Hour)
	r.chain.minedNonce = 5
	r.reap(rp)
	if r.get("k").Status != store.StatusConfirmed || len(r.ledger.released) != 0 || len(*alerts) != 0 {
		t.Fatal("touched a confirmed row")
	}
}

func signReq(w store.Withdrawal) wallet.SignRequest {
	return wallet.SignRequest{ToAddress: w.ToAddress, AssetID: w.AssetID, Amount: w.Amount.String(), ChainID: chainID, IdempotencyKey: w.Key()}
}
