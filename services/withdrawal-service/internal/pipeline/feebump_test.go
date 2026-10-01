package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/pkg/events"
	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

func (r *rig) alerts() *[]string {
	var a []string
	r.p.Alert = func(m string) { a = append(a, m) }
	return &a
}

func hashOf(t *testing.T, raw string) common.Hash {
	t.Helper()
	h, err := chain.TxHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (r *rig) txs(w store.Withdrawal) []store.Tx {
	r.t.Helper()
	txs, err := r.st.ListTxs(context.Background(), w.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return txs
}

// A node that refuses the tx as underpriced triggers a Fee Bump; the replacement is what goes out.
func TestUnderpricedBroadcastIsFeeBumped(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k")
	r.chain.minFee = big.NewInt(3) // the fake's original pays 2
	_ = r.step()
	got := r.get("k")
	if got.Status != store.StatusSigned || len(r.chain.sent) != 0 {
		t.Fatalf("after the refusal: %s, sent %d", got.Status, len(r.chain.sent))
	}
	txs := r.txs(got)
	if len(txs) != 1 || txs[0].Seq != 1 || r.signer.replaceCalls[0] != w.Key()+"/1" {
		t.Fatalf("stored %+v, replace calls %v", txs, r.signer.replaceCalls)
	}
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got = r.get("k")
	if got.Status != store.StatusBroadcast || *got.TxHash != txs[0].Hash || len(r.chain.sent) != 1 || r.chain.sent[0] != txs[0].Raw {
		t.Fatalf("after the bump: %s tx_hash %v sent %d", got.Status, got.TxHash, len(r.chain.sent))
	}
	if got.SignedRawTx == nil || *got.SentHead != 100 || *got.Nonce != 0 {
		t.Fatalf("%+v", got)
	}
	b, err := events.Decode[events.WithdrawalBroadcast](r.pub.msgs[0].value)
	if err != nil || b.TxHash != txs[0].Hash || b.Nonce != 0 {
		t.Fatalf("broadcast event %+v %v", b, err)
	}
}

// Each node words "your max fee is too low" differently; all of them are a Fee Bump's cue.
func TestEveryUnderpricedWordingTriggersAFeeBump(t *testing.T) {
	for _, msg := range []string{
		"replacement transaction underpriced", "transaction underpriced", "fee too low",
		"server returned an error response: error code -32003: max fee per gas less than block base fee",
	} {
		r := newRig(t)
		r.reserved("k")
		r.chain.broadcastErr = errors.New(msg)
		_ = r.step()
		if got := r.get("k"); got.Status != store.StatusSigned || len(r.txs(got)) != 1 {
			t.Errorf("%q: status %s, %d Fee Bumps stored, want signed and 1", msg, got.Status, len(r.txs(got)))
		}
	}
	r := newRig(t)
	r.reserved("k")
	r.chain.broadcastErr = errors.New("connection refused")
	_ = r.step()
	if len(r.signer.replaceCalls) != 0 {
		t.Fatalf("a connection error triggered a Fee Bump: %v", r.signer.replaceCalls)
	}
}

// Not mined 20 blocks after it was sent AND underpriced → bump; at most 5; then an ALERT.
func TestStuckUnderpricedTxIsBumpedAfter20BlocksUpToFiveTimes(t *testing.T) {
	r := newRig(t)
	alerts := r.alerts()
	w := r.broadcastOnly("k") // sent at head 100, fee cap 2
	r.chain.baseFee = 1000

	r.chain.head = 119
	_ = r.step()
	if len(r.signer.replaceCalls) != 0 {
		t.Fatal("bumped before 20 blocks")
	}
	for i := 1; i <= 5; i++ {
		r.chain.head = 100 + int64(20*i)
		_ = r.step() // stores bump i
		if got := r.get("k"); len(r.txs(got)) != i || got.SentHead != nil {
			t.Fatalf("bump %d: stored %d, sent_head %v (want nil: not sent yet)", i, len(r.txs(got)), got.SentHead)
		}
		if err := r.step(); err != nil { // sends it, stamps the head
			t.Fatal(err)
		}
		got := r.get("k")
		if got.SentHead == nil || *got.SentHead != r.chain.head || got.Status != store.StatusBroadcast {
			t.Fatalf("bump %d: sent_head %v status %s", i, got.SentHead, got.Status)
		}
		if last := r.txs(got)[i-1]; r.chain.sent[len(r.chain.sent)-1] != last.Raw {
			t.Fatalf("bump %d: the latest version was not the one sent", i)
		}
	}
	// 19 blocks after the 5th bump nothing happens; at 20 the limit is an ALERT, not a 6th bump.
	r.chain.head += 19
	_ = r.step()
	r.chain.head++
	_ = r.step()
	_ = r.step()
	if len(r.signer.replaceCalls) != 5 || len(r.txs(r.get("k"))) != 5 {
		t.Fatalf("replace calls %v", r.signer.replaceCalls)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "5 Fee Bumps") || !strings.Contains((*alerts)[0], w.ID) {
		t.Fatalf("alerts = %q", *alerts)
	}
	if len(r.ledger.released) != 0 {
		t.Fatal("released while still possibly mineable")
	}
}

func TestAStuckTxThatIsNotUnderpricedIsNotBumped(t *testing.T) {
	r := newRig(t)
	r.broadcastOnly("k")
	r.chain.baseFee = 1 // the tx pays 2: fine, the network is just slow
	r.chain.head = 100 + 500
	_ = r.step()
	_ = r.step()
	if len(r.signer.replaceCalls) != 0 {
		t.Fatalf("bumped a tx that is not underpriced: %v", r.signer.replaceCalls)
	}
}

// Any version may be the one that is mined; its hash becomes tx_hash and goes out in withdrawals.confirmed.
func TestConfirmedWithAFeeBumpUsesThatHash(t *testing.T) {
	for _, minedSeq := range []int{0, 1} {
		t.Run(fmt.Sprintf("version %d mined", minedSeq), func(t *testing.T) {
			r := newRig(t)
			w := r.broadcastOnly("k")
			origHash := strings.ToLower(hashOf(t, *w.SignedRawTx).Hex())
			r.chain.baseFee = 1000
			r.chain.head = 120
			_ = r.step() // bump stored
			_ = r.step() // bump sent
			txs := r.txs(r.get("k"))
			if len(txs) != 1 {
				t.Fatal("setup: no bump")
			}
			mined := origHash
			if minedSeq == 1 {
				mined = txs[0].Hash
			}
			r.chain.receipts[common.HexToHash(mined)] = &chain.Receipt{BlockNumber: 121, Success: true}
			r.chain.head = 121 + depth
			if err := r.step(); err != nil {
				t.Fatal(err)
			}
			got := r.get("k")
			if got.Status != store.StatusConfirmed || *got.TxHash != mined || len(r.ledger.settled) != 1 {
				t.Fatalf("%s tx_hash %v, want %s", got.Status, got.TxHash, mined)
			}
			var confirmed events.WithdrawalConfirmed
			for _, m := range r.pub.msgs {
				if m.topic == events.TopicWithdrawalsConfirmed {
					confirmed, _ = events.Decode[events.WithdrawalConfirmed](m.value)
				}
			}
			if confirmed.TxHash != mined {
				t.Fatalf("withdrawals.confirmed carries %q, want %s", confirmed.TxHash, mined)
			}
			b, _ := events.Decode[events.WithdrawalBroadcast](r.pub.msgs[0].value)
			if b.TxHash != origHash {
				t.Fatalf("withdrawals.broadcast carries %q, want the original %s", b.TxHash, origHash)
			}
		})
	}
}

func TestFeeBumpRefusalsAreAlertsAndTransientErrorsRetryTheSameSeq(t *testing.T) {
	setup := func(t *testing.T) (*rig, *[]string) {
		r := newRig(t)
		a := r.alerts()
		r.broadcastOnly("k")
		r.chain.baseFee = 1000
		r.chain.head = 120
		return r, a
	}
	t.Run("fee ceiling → ALERT, nothing stored", func(t *testing.T) {
		r, a := setup(t)
		r.signer.replaceErr = fmt.Errorf("%w: too dear", wallet.ErrReplaceCeiling)
		_ = r.step()
		if len(*a) != 1 || !strings.Contains((*a)[0], "ceiling") || len(r.txs(r.get("k"))) != 0 {
			t.Fatalf("alerts %q", *a)
		}
	})
	t.Run("refused (a Nonce Filler is the latest) → ALERT", func(t *testing.T) {
		r, a := setup(t)
		r.signer.replaceErr = fmt.Errorf("%w (409)", wallet.ErrReplaceRefused)
		_ = r.step()
		if len(*a) != 1 || !strings.Contains((*a)[0], "refused") {
			t.Fatalf("alerts %q", *a)
		}
	})
	t.Run("transient → retried with the same seq", func(t *testing.T) {
		r, a := setup(t)
		r.signer.replaceErr = errors.New("wallet-service replace answered 503")
		if err := r.step(); err == nil {
			t.Fatal("a transient failure should surface")
		}
		r.signer.replaceErr = nil
		_ = r.step()
		w := r.get("k")
		if len(*a) != 0 || len(r.txs(w)) != 1 {
			t.Fatalf("alerts %q stored %d", *a, len(r.txs(w)))
		}
		for _, c := range r.signer.replaceCalls {
			if !strings.HasSuffix(c, "/1") {
				t.Fatalf("replace calls %v: every retry must ask for seq 1", r.signer.replaceCalls)
			}
		}
	})
}

// Defence in depth: a "replacement" that is not the same tx is never stored or sent.
func TestAReplacementThatRedirectsFundsIsNeverStored(t *testing.T) {
	r := newRig(t)
	a := r.alerts()
	r.broadcastOnly("k")
	r.chain.baseFee = 1000
	r.chain.head = 120
	r.signer.corrupt = true
	_ = r.step()
	_ = r.step()
	if len(*a) != 1 || !strings.Contains((*a)[0], "not the same tx") || len(r.txs(r.get("k"))) != 0 || len(r.chain.sent) != 1 {
		t.Fatalf("alerts %q, stored %d, sent %d", *a, len(r.txs(r.get("k"))), len(r.chain.sent))
	}
}

// invariant #23: before signing, the whitelist entry and the Screening List are re-checked.
func TestRecheckFailureBeforeSigningVoidsReleasesAndFails(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k")
	v := &fakeVoider{res: wallet.VoidResult{Voided: true}}
	r.p.Voider = v
	r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_not_whitelisted", nil }
	_ = r.step()
	got := r.get("k")
	if got.Status != store.StatusFailed || *got.RejectReason != "destination_not_whitelisted" ||
		len(r.ledger.released) != 1 || r.ledger.released[0] != w.Key() || len(v.calls) != 1 {
		t.Fatalf("%+v released %v voids %v", got, r.ledger.released, v.calls)
	}
	if r.signer.next != 0 || len(r.signer.keys) != 0 {
		t.Fatal("a request that failed the re-check was signed")
	}
}

// A lost sign reply means a nonce may exist even though the row is `reserved`: then the re-check
// must not fail the request — the tx goes out unless an operator cancels it.
func TestRecheckFailureAfterAnUnnoticedSignatureDoesNotBlockIt(t *testing.T) {
	r := newRig(t)
	a := r.alerts()
	w := r.reserved("k")
	raw, nonce, _ := r.signer.SignHotWallet(context.Background(), signReq(w))
	r.signer.keys = nil
	r.p.Voider = &fakeVoider{res: wallet.VoidResult{RawTx: raw, Nonce: nonce}}
	r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_on_screening_list", nil }
	_ = r.step()
	got := r.get("k")
	if got.Status != store.StatusBroadcast || len(r.ledger.released) != 0 || len(r.chain.sent) != 1 {
		t.Fatalf("%s released %v sent %d", got.Status, r.ledger.released, len(r.chain.sent))
	}
	if len(*a) != 1 || !strings.Contains((*a)[0], "hot-wallet cancel") {
		t.Fatalf("alerts %q", *a)
	}
}

func TestRecheckEdgeCases(t *testing.T) {
	t.Run("passes → signs as usual", func(t *testing.T) {
		r := newRig(t)
		r.reserved("k")
		r.p.Voider = &fakeVoider{}
		r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "", nil }
		_ = r.step()
		if r.get("k").Status != store.StatusBroadcast {
			t.Fatal("did not proceed")
		}
	})
	t.Run("no verdict → nothing signed, nothing released", func(t *testing.T) {
		r := newRig(t)
		r.reserved("k")
		r.p.Voider = &fakeVoider{}
		r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "", errors.New("db down") }
		if err := r.step(); err == nil {
			t.Fatal("expected the failure to surface")
		}
		if r.get("k").Status != store.StatusReserved || len(r.signer.keys) != 0 || len(r.ledger.released) != 0 {
			t.Fatal("moved without a verdict")
		}
	})
	t.Run("fails but wallet-service cannot void → ALERT only", func(t *testing.T) {
		r := newRig(t)
		a := r.alerts()
		r.reserved("k")
		r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_not_whitelisted", nil }
		_ = r.step()
		if r.get("k").Status != store.StatusReserved || len(r.signer.keys) != 0 || len(r.ledger.released) != 0 || len(*a) != 1 {
			t.Fatalf("status %s alerts %q", r.get("k").Status, *a)
		}
	})
	t.Run("void fails → retried, never released", func(t *testing.T) {
		r := newRig(t)
		r.reserved("k")
		r.p.Voider = &fakeVoider{err: errors.New("503")}
		r.p.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_not_whitelisted", nil }
		_ = r.step()
		if r.get("k").Status != store.StatusReserved || len(r.ledger.released) != 0 {
			t.Fatal("released without the void")
		}
	})
}

// A sign answered 410 means the Reaper voided the key: nothing to do here, never a Release.
func TestSignOfAVoidedKeyIsLeftToTheReaper(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	r.signer.voided = true
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if r.get("k").Status != store.StatusReserved || len(r.ledger.released) != 0 {
		t.Fatal("the pipeline acted on a voided key")
	}
}

// The Reaper's proof counts every version: our Fee Bump being mined means the nonce is NOT consumed by another tx.
func TestReaperDoesNotReleaseWhenAFeeBumpIsMined(t *testing.T) {
	r := newRig(t)
	r.broadcastOnly("k")
	r.chain.baseFee = 1000
	r.chain.head = 120
	_ = r.step()
	_ = r.step()
	txs := r.txs(r.get("k"))
	r.chain.receipts[common.HexToHash(txs[0].Hash)] = &chain.Receipt{BlockNumber: 121, Success: true}
	r.chain.minedNonce = 1
	rp, _ := r.reaper(0)
	r.reap(rp)
	r.chain.head += 500
	r.reap(rp)
	if r.get("k").Status != store.StatusBroadcast || len(r.ledger.released) != 0 {
		t.Fatal("released a request whose Fee Bump was mined")
	}
	// ... and with no receipt anywhere, the same facts do release it.
	delete(r.chain.receipts, common.HexToHash(txs[0].Hash))
	r.reap(rp)
	if got := r.get("k"); got.Status != store.StatusFailed || *got.RejectReason != ReasonReplacedOrDropped {
		t.Fatalf("%+v", got)
	}
}

// Nonce Filler: once the operator's filler consumed nonce N, the Reaper releases the signed request.
func TestReaperCompletesANonceFiller(t *testing.T) {
	r := newRig(t)
	r.signedOnly("k")
	r.chain.minedNonce = 1 // the filler (a tx we never see a receipt for) was mined at nonce 0
	rp, _ := r.reaper(0)
	r.reap(rp)
	if got := r.get("k"); got.Status != store.StatusFailed || *got.RejectReason != ReasonReplacedOrDropped || len(r.ledger.released) != 1 {
		t.Fatalf("%+v released %v", got, r.ledger.released)
	}
}

func TestReaperAlertsAboutASignedRequestThatNoLongerPassesTheRecheck(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k")
	rp, alerts := r.reaper(stuck)
	rp.Recheck = func(context.Context, store.Withdrawal) (string, error) { return "destination_on_screening_list", nil }
	r.reap(rp)
	joined := strings.Join(*alerts, "\n")
	if !strings.Contains(joined, "no longer passes the pre-sign re-check") || !strings.Contains(joined, "hot-wallet cancel --key "+w.Key()) {
		t.Fatalf("alerts = %q", *alerts)
	}
	if r.get("k").Status != store.StatusSigned || len(r.ledger.released) != 0 {
		t.Fatal("a signed request was blocked")
	}
}

// `abandon` must refuse when ANY version of the tx is mined — a Fee Bump may be what paid out.
type disputedStore struct {
	w   store.Withdrawal
	txs []store.Tx
}

func (d disputedStore) GetByID(context.Context, string) (store.Withdrawal, error) { return d.w, nil }
func (d disputedStore) ListTxs(context.Context, string) ([]store.Tx, error)       { return d.txs, nil }
func (d disputedStore) MarkFailed(context.Context, string, string, string, string, *store.GasRecord, func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error) {
	return d.w, nil
}

func TestAbandonRefusesWhenAFeeBumpIsMined(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k")
	from := store.StatusBroadcast
	w.Status, w.DisputedFrom = store.StatusDisputed, &from
	rep, _ := r.signer.ReplaceHotWallet(context.Background(), w.Key(), 1)
	r.chain.receipts[common.HexToHash(rep.Hash)] = &chain.Receipt{BlockNumber: 5, Success: true}
	r.chain.minedNonce = 5
	_, err := Abandon(context.Background(), disputedStore{w: w, txs: []store.Tx{{Seq: 1, Hash: rep.Hash, Raw: rep.RawTx}}}, r.ledger, r.chain, nil, w.ID)
	if err == nil || !strings.Contains(err.Error(), "is mined") || len(r.ledger.released) != 0 {
		t.Fatalf("abandon = %v released %v", err, r.ledger.released)
	}
}
