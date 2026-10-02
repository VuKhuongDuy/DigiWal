package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

// failedEvents are the withdrawals.failed messages the pipeline has published so far, after flushing the outbox.
func (r *rig) failedEvents() []events.WithdrawalFailed {
	r.t.Helper()
	if err := r.p.publish(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	var out []events.WithdrawalFailed
	for _, m := range r.pub.msgs {
		if m.topic != events.TopicWithdrawalsFailed {
			continue
		}
		e, err := events.Decode[events.WithdrawalFailed](m.value)
		if err != nil {
			r.t.Fatal(err)
		}
		if m.key != e.WithdrawalID {
			r.t.Fatalf("Kafka key %q, want the withdrawal_id %q", m.key, e.WithdrawalID)
		}
		out = append(out, e)
	}
	return out
}

func (r *rig) onlyFailedEvent(want string) events.WithdrawalFailed {
	r.t.Helper()
	evs := r.failedEvents()
	if len(evs) != 1 || evs[0].Reason != want {
		r.t.Fatalf("withdrawals.failed events = %+v, want exactly one with reason %s", evs, want)
	}
	return evs[0]
}

// A request that never reached wallet-service has no tx and no nonce in its event.
func TestSignRefusedPublishesOneFailedEventWithoutATx(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k")
	r.signer.refuse = true
	_ = r.step()
	_ = r.step()
	e := r.onlyFailedEvent(ReasonSignRefused)
	if e.WithdrawalID != w.ID || e.IdempotencyKey != "k" || e.UserID != "alice" || e.TxHash != "" || e.Nonce != nil || e.ChainID != chainID || e.RevertReason != "" {
		t.Fatalf("%+v", e)
	}
	_ = r.step()
	if len(r.failedEvents()) != 1 {
		t.Fatal("a later pass published the failure again")
	}
}

func TestRecheckFailurePublishesTheReason(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	r.p.Voider = &fakeVoider{res: wallet.VoidResult{Voided: true}}
	r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_on_screening_list", nil }
	_ = r.step()
	r.onlyFailedEvent("destination_on_screening_list")
}

// A reverted tx's event names the mined tx and the revert reason; a destination at fault is blocked.
func TestRevertedTxPublishesTheRevertReasonAndBlocksTheDestination(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, _, _ = r.st.AddWhitelistEntry(ctx, "alice", toAddr)
	w := r.reserved("k")
	_ = r.step()
	sent := r.get("k")
	r.chain.mine(false)
	r.chain.revertReason = "execution reverted: recipient rejects ETH"
	r.chain.head += depth
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	e := r.onlyFailedEvent(ReasonTxReverted)
	if e.RevertReason != "execution reverted: recipient rejects ETH" || e.TxHash != *sent.TxHash || e.Nonce == nil || *e.Nonce != 0 || e.WithdrawalID != w.ID {
		t.Fatalf("%+v", e)
	}
	if st, ok, _ := r.st.WhitelistStatus(ctx, "alice", toAddr, time.Hour); !ok || st != store.WhitelistBlocked {
		t.Fatalf("destination entry is %q, want blocked", st)
	}
}

// Our own operational faults, and reverts we cannot explain, never block the user's destination.
func TestRevertsThatAreNotTheDestinationsFaultDoNotBlockIt(t *testing.T) {
	for name, reason := range map[string]string{
		"Hot Wallet short of the token": "execution reverted: ERC20: transfer amount exceeds balance",
		"Hot Wallet short of ETH":       "insufficient funds for gas * price + value",
		"cause not recovered":           "",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			ctx := context.Background()
			_, _, _ = r.st.AddWhitelistEntry(ctx, "alice", toAddr)
			r.reserved("k")
			_ = r.step()
			r.chain.mine(false)
			r.chain.revertReason = reason
			r.chain.head += depth
			_ = r.step()
			e := r.onlyFailedEvent(ReasonTxReverted)
			if e.RevertReason != reason {
				t.Errorf("revert_reason %q, want %q", e.RevertReason, reason)
			}
			if st, _, _ := r.st.WhitelistStatus(ctx, "alice", toAddr, time.Hour); st == store.WhitelistBlocked {
				t.Error("the destination was blocked for a fault that is not its own")
			}
		})
	}
}

// A Fee Bump version is the "latest known tx" of a request that then fails without being mined.
func TestEveryReaperFailurePublishesExactlyOneEvent(t *testing.T) {
	r := newRig(t)
	r.newRequest("stale-req")
	r.validated("stale-validated")
	r.reserved("stale-reserved")
	dropped := r.signedOnly("dropped")
	r.chain.minedNonce = uint64(*dropped.Nonce) + 1 // nonce consumed by something else
	rp, _ := r.reaper(stuck)
	rp.Voider = &fakeVoider{res: wallet.VoidResult{Voided: true}}
	r.reap(rp)
	r.reap(rp) // a second pass must not publish anything again

	got := map[string]events.WithdrawalFailed{}
	for _, e := range r.failedEvents() {
		if _, dup := got[e.IdempotencyKey]; dup {
			t.Fatalf("two events for %s", e.IdempotencyKey)
		}
		got[e.IdempotencyKey] = e
	}
	want := map[string]string{"stale-req": ReasonStaleRequest, "stale-validated": ReasonStaleReservation,
		"stale-reserved": ReasonStaleReservation, "dropped": ReasonReplacedOrDropped}
	if len(got) != len(want) {
		t.Fatalf("events for %v, want %v", keys(got), want)
	}
	for k, reason := range want {
		if got[k].Reason != reason {
			t.Errorf("%s: reason %q, want %q", k, got[k].Reason, reason)
		}
	}
	// Only the signed one has a tx and a nonce to report.
	if d := got["dropped"]; d.TxHash == "" || d.Nonce == nil || strings.ToLower(d.TxHash) != d.TxHash {
		t.Errorf("dropped: %+v", d)
	}
	if got["stale-req"].TxHash != "" || got["stale-req"].Nonce != nil {
		t.Errorf("stale-req: %+v", got["stale-req"])
	}
}

func keys(m map[string]events.WithdrawalFailed) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Abandoning a Settled, reorged-out request is a `failed` too (confirmed → ... → failed).
func TestAbandonPublishesTheFailedEvent(t *testing.T) {
	r := newRig(t)
	w := r.disputeConfirmed("a")
	r.chain.minedNonce = uint64(*w.Nonce) + 1
	if _, err := abandon(r, w.ID); err != nil {
		t.Fatal(err)
	}
	e := r.onlyFailedEvent(ReasonAbandonedRefunded)
	if e.WithdrawalID != w.ID || e.TxHash == "" || e.Nonce == nil {
		t.Fatalf("%+v", e)
	}
}
