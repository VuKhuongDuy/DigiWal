package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
)

type fakeFillers struct {
	hashes []string
	err    error
}

func (f fakeFillers) CancelHashes(context.Context, string) ([]string, error) { return f.hashes, f.err }

func (r *rig) gasPending() []store.GasPending {
	r.t.Helper()
	g, err := r.st.ListGasPending(context.Background(), 100)
	if err != nil {
		r.t.Fatal(err)
	}
	return g
}

// ledger.md invariant #12: after Settle, the mined tx's gas is posted once, with the receipt's numbers.
func TestConfirmedWithdrawalPostsItsGasOnce(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k")
	_ = r.step()
	hash := *r.get("k").TxHash
	r.chain.mine(true)
	r.chain.head += depth
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if got := r.get("k"); got.Status != store.StatusConfirmed {
		t.Fatalf("%s", got.Status)
	}
	if len(r.ledger.gasPosts) != 1 {
		t.Fatalf("gas posts = %+v", r.ledger.gasPosts)
	}
	g := r.ledger.gasPosts[0]
	if g.TxHash != hash || g.GasUsed != 21000 || g.GasPrice.Cmp(big.NewInt(2_000_000_000)) != 0 || g.Purpose != "withdrawal" || g.ChainID != chainID {
		t.Fatalf("%+v (withdrawal %s)", g, w.ID)
	}
	if len(r.gasPending()) != 0 {
		t.Fatal("still pending after the ledger accepted it")
	}
	_ = r.step()
	_ = r.step()
	if len(r.ledger.gasPosts) != 1 {
		t.Fatalf("posted again: %d", len(r.ledger.gasPosts))
	}
}

// The numbers are written with the state change, so a ledger outage — or a crash — never loses them.
func TestGasIsRetriedUntilTheLedgerAnswers(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	r.ledger.gasErr = errors.New("ledger-service gas-fees answered 503")
	if err := r.step(); err == nil {
		t.Fatal("a ledger outage should surface")
	}
	if r.get("k").Status != store.StatusConfirmed || len(r.gasPending()) != 1 {
		t.Fatalf("status %s, pending %d: the gas must stay recorded", r.get("k").Status, len(r.gasPending()))
	}
	// "Crash": a brand-new pipeline over the same database finishes the job.
	p2 := *r.p
	r.ledger.gasErr = nil
	if err := p2.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.ledger.gasPosts) != 1 || len(r.gasPending()) != 0 {
		t.Fatalf("posts %d, pending %d", len(r.ledger.gasPosts), len(r.gasPending()))
	}
}

// Only the version that was mined pays: a Fee Bump's unmined original posts nothing.
func TestOnlyTheMinedFeeBumpVersionPostsGas(t *testing.T) {
	r := newRig(t)
	w := r.broadcastOnly("k")
	orig := strings.ToLower(hashOf(t, *w.SignedRawTx).Hex())
	r.chain.baseFee = 1000
	r.chain.head = 120
	_ = r.step()
	_ = r.step()
	bump := r.txs(r.get("k"))[0].Hash
	r.chain.receipts[common.HexToHash(bump)] = &chain.Receipt{BlockNumber: 121, Success: true, GasUsed: 30000, EffectiveGasPrice: big.NewInt(77)}
	r.chain.head = 121 + depth
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if len(r.ledger.gasPosts) != 1 || r.ledger.gasPosts[0].TxHash != bump || r.ledger.gasPosts[0].TxHash == orig ||
		r.ledger.gasPosts[0].GasUsed != 30000 || r.ledger.gasPosts[0].GasPrice.Int64() != 77 {
		t.Fatalf("gas posts = %+v, want only the mined bump %s", r.ledger.gasPosts, bump)
	}
}

// A reverted tx still spent gas: posted as withdrawal_reverted, with the mined tx; the user is Released in full.
func TestRevertedTxPostsItsGasAsWithdrawalReverted(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	_ = r.step()
	hash := *r.get("k").TxHash
	r.chain.mine(false)
	r.chain.revertReason = "execution reverted: nope"
	r.chain.head += depth
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if len(r.ledger.gasPosts) != 1 || r.ledger.gasPosts[0].Purpose != "withdrawal_reverted" || r.ledger.gasPosts[0].TxHash != hash || len(r.ledger.settled) != 0 {
		t.Fatalf("gas posts %+v settled %v", r.ledger.gasPosts, r.ledger.settled)
	}
}

// A request that never mined anything (stale, sign refused) spent no gas and posts nothing.
func TestFailuresWithoutAMinedTxPostNothing(t *testing.T) {
	r := newRig(t)
	r.reserved("k")
	r.signer.refuse = true
	_ = r.step()
	_ = r.step()
	if r.get("k").Status != store.StatusFailed || len(r.ledger.gasPosts) != 0 || len(r.gasPending()) != 0 {
		t.Fatalf("status %s, posts %v", r.get("k").Status, r.ledger.gasPosts)
	}
}

// A permanent refusal (409/400) is an ALERT, never a retry storm, and never marks the gas posted.
func TestARefusedGasFeeIsAnAlertNotALoop(t *testing.T) {
	r := newRig(t)
	a := r.alerts()
	r.reserved("k")
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	r.ledger.gasErr = fmt.Errorf("%w (409): different values", ledger.ErrGasFeeRefused)
	_ = r.step()
	_ = r.step()
	if len(*a) != 1 || !strings.Contains((*a)[0], "refused") || len(r.gasPending()) != 1 {
		t.Fatalf("alerts %q, pending %d", *a, len(r.gasPending()))
	}
}

// Nonce Filler: once the Reaper proves the nonce consumed, the filler's own gas is recorded (purpose nonce_filler).
func TestReaperRecordsTheGasOfAMinedNonceFiller(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k")
	filler := "0x" + strings.Repeat("ab", 32)
	r.chain.receipts[common.HexToHash(filler)] = &chain.Receipt{BlockNumber: 9, Success: true, GasUsed: 21000, EffectiveGasPrice: big.NewInt(5)}
	r.chain.minedNonce = uint64(*w.Nonce) + 1
	rp, _ := r.reaper(0)
	rp.Fillers = fakeFillers{hashes: []string{filler}}
	r.reap(rp)
	if got := r.get("k"); got.Status != store.StatusFailed || *got.RejectReason != ReasonReplacedOrDropped {
		t.Fatalf("%+v", got)
	}
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if len(r.ledger.gasPosts) != 1 || r.ledger.gasPosts[0].Purpose != "nonce_filler" || r.ledger.gasPosts[0].TxHash != filler || r.ledger.gasPosts[0].GasPrice.Int64() != 5 {
		t.Fatalf("gas posts = %+v", r.ledger.gasPosts)
	}
}

func TestReaperPostsNoGasWhenAnotherTxConsumedTheNonce(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k")
	r.chain.minedNonce = uint64(*w.Nonce) + 1
	rp, _ := r.reaper(0)
	rp.Fillers = fakeFillers{} // no filler of ours exists
	r.reap(rp)
	_ = r.step()
	if r.get("k").Status != store.StatusFailed || len(r.ledger.gasPosts) != 0 {
		t.Fatalf("gas posts %+v", r.ledger.gasPosts)
	}
	// A filler that was signed but never mined spent nothing either.
	r2 := newRig(t)
	w2 := r2.signedOnly("k")
	r2.chain.minedNonce = uint64(*w2.Nonce) + 1
	rp2, _ := r2.reaper(0)
	rp2.Fillers = fakeFillers{hashes: []string{"0x" + strings.Repeat("cd", 32)}}
	r2.reap(rp2)
	_ = r2.step()
	if len(r2.ledger.gasPosts) != 0 {
		t.Fatalf("an unmined filler posted gas: %+v", r2.ledger.gasPosts)
	}
}

// If wallet-service cannot say which fillers exist, the Release waits: the proof still holds next pass.
func TestReaperWaitsRatherThanLoseAFillersGas(t *testing.T) {
	r := newRig(t)
	w := r.signedOnly("k")
	r.chain.minedNonce = uint64(*w.Nonce) + 1
	rp, _ := r.reaper(0)
	rp.Fillers = fakeFillers{err: errors.New("wallet-service down")}
	if err := rp.Step(context.Background()); err == nil {
		t.Fatal("expected the lookup failure to surface")
	}
	if r.get("k").Status != store.StatusSigned || len(r.ledger.released) != 0 {
		t.Fatal("released without knowing about the filler")
	}
	rp.Fillers = fakeFillers{}
	r.reap(rp)
	if r.get("k").Status != store.StatusFailed {
		t.Fatal("not released once the lookup works")
	}
}
