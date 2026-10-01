package pipeline

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"digiwal/pkg/events"
	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/testdb"
	"digiwal/services/withdrawal-service/internal/wallet"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	chainID = int64(31337)
	depth   = int64(3)
	toAddr  = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
)

// ---- fakes ---------------------------------------------------------------------------------

// fakeSigner behaves like wallet-service: one nonce per key, the same signed tx on every retry
// (invariant #16), and a real, decodable EIP-1559 tx.
type fakeSigner struct {
	key       *ecdsa.PrivateKey
	byKey     map[string]string
	nonces    map[string]uint64
	next      uint64
	keys      []string
	refuse    bool
	conflict  bool
	failNext  int  // transient failures before answering
	loseReply bool // sign and store, then fail as if the response were lost
	voided    bool // answer 410: the Reaper voided the key

	reps         map[string][]wallet.Replacement // Fee Bumps per key, like wallet-service stores them
	replaceErr   error
	replaceCalls []string // "key/seq"
	corrupt      bool     // build replacements that redirect the funds (must never be accepted)
}

func newSigner() *fakeSigner {
	k, _ := crypto.GenerateKey()
	return &fakeSigner{key: k, byKey: map[string]string{}, nonces: map[string]uint64{}}
}

func (s *fakeSigner) SignHotWallet(_ context.Context, r wallet.SignRequest) (string, uint64, error) {
	s.keys = append(s.keys, r.IdempotencyKey)
	if s.voided {
		return "", 0, wallet.ErrKeyVoided
	}
	if s.refuse {
		return "", 0, wallet.ErrSignRefused
	}
	if s.conflict {
		return "", 0, wallet.ErrKeyConflict
	}
	if s.failNext > 0 {
		s.failNext--
		return "", 0, errors.New("wallet-service: 503")
	}
	if raw, ok := s.byKey[r.IdempotencyKey]; ok {
		return raw, s.nonces[r.IdempotencyKey], nil
	}
	amount, _ := new(big.Int).SetString(r.Amount, 10)
	to := common.HexToAddress(r.ToAddress)
	tx, _ := types.SignNewTx(s.key, types.LatestSignerForChainID(big.NewInt(r.ChainID)), &types.DynamicFeeTx{
		ChainID: big.NewInt(r.ChainID), Nonce: s.next, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2), Gas: 21000, To: &to, Value: amount,
	})
	b, _ := tx.MarshalBinary()
	raw := hexutil.Encode(b)
	s.byKey[r.IdempotencyKey], s.nonces[r.IdempotencyKey] = raw, s.next
	s.next++
	if s.loseReply {
		s.loseReply = false
		return "", 0, errors.New("context deadline exceeded")
	}
	return raw, s.nonces[r.IdempotencyKey], nil
}

// ReplaceHotWallet behaves like wallet-service: the same tx at the same nonce, fee × 1.125 + 1; the
// same seq replays; a gap is refused.
func (s *fakeSigner) ReplaceHotWallet(_ context.Context, key string, seq int) (wallet.Replacement, error) {
	s.replaceCalls = append(s.replaceCalls, fmt.Sprintf("%s/%d", key, seq))
	if s.replaceErr != nil {
		return wallet.Replacement{}, s.replaceErr
	}
	if s.reps == nil {
		s.reps = map[string][]wallet.Replacement{}
	}
	if seq >= 1 && seq <= len(s.reps[key]) {
		return s.reps[key][seq-1], nil
	}
	if seq != len(s.reps[key])+1 {
		return wallet.Replacement{}, wallet.ErrReplaceRefused
	}
	latest := s.byKey[key]
	if n := len(s.reps[key]); n > 0 {
		latest = s.reps[key][n-1].RawTx
	}
	prev, _ := chain.Decode(latest)
	to := *prev.To()
	if s.corrupt {
		to = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	}
	tx, _ := types.SignNewTx(s.key, types.LatestSignerForChainID(prev.ChainId()), &types.DynamicFeeTx{
		ChainID: prev.ChainId(), Nonce: prev.Nonce(), GasTipCap: new(big.Int).Add(prev.GasTipCap(), big.NewInt(1)),
		GasFeeCap: new(big.Int).Add(new(big.Int).Div(new(big.Int).Mul(prev.GasFeeCap(), big.NewInt(1125)), big.NewInt(1000)), big.NewInt(1)),
		Gas:       prev.Gas(), To: &to, Value: prev.Value(), Data: prev.Data(),
	})
	b, _ := tx.MarshalBinary()
	rep := wallet.Replacement{RawTx: hexutil.Encode(b), Hash: strings.ToLower(tx.Hash().Hex()), Nonce: prev.Nonce(), Seq: seq}
	s.reps[key] = append(s.reps[key], rep)
	return rep, nil
}

type fakeChain struct {
	minedNonce   uint64
	revertReason string
	head         int64
	receipts     map[common.Hash]*chain.Receipt
	sent         []string
	broadcastErr error
	baseFee      int64
	balance      *big.Int
	minFee       *big.Int // the node refuses a tx whose max fee is below this as underpriced
	pool         map[common.Hash]bool
}

func newChain() *fakeChain { return &fakeChain{head: 100, receipts: map[common.Hash]*chain.Receipt{}} }

func (c *fakeChain) ChainID(context.Context) (int64, error) { return chainID, nil }
func (c *fakeChain) Head(context.Context) (int64, error)    { return c.head, nil }
func (c *fakeChain) Broadcast(_ context.Context, raw string) error {
	if c.broadcastErr != nil {
		return c.broadcastErr
	}
	if c.minFee != nil {
		if tx, err := chain.Decode(raw); err == nil && tx.GasFeeCap().Cmp(c.minFee) < 0 {
			return errors.New("replacement transaction underpriced")
		}
	}
	c.sent = append(c.sent, raw)
	return nil
}
func (c *fakeChain) Receipt(_ context.Context, h common.Hash) (*chain.Receipt, error) {
	return c.receipts[h], nil
}

func (c *fakeChain) MinedNonce(context.Context, common.Address) (uint64, error) {
	return c.minedNonce, nil
}

func (c *fakeChain) BaseFee(context.Context) (*big.Int, error) { return big.NewInt(c.baseFee), nil }

func (c *fakeChain) Balance(context.Context, common.Address) (*big.Int, error) {
	if c.balance == nil {
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil), nil // plenty
	}
	return c.balance, nil
}

func (c *fakeChain) RevertReason(context.Context, common.Hash, int64) (string, error) {
	return c.revertReason, nil
}

// mine puts every sent tx in the current head block.
func (c *fakeChain) mine(success bool) {
	for _, raw := range c.sent {
		h, _ := chain.TxHash(raw)
		if c.receipts[h] == nil {
			c.receipts[h] = &chain.Receipt{BlockNumber: c.head, Success: success}
		}
	}
}

type fakeLedger struct {
	settled, released []string
	refunds           map[string][]string
	settleErr         error
}

func (l *fakeLedger) Refund(_ context.Context, key string, entries []string) error {
	if l.refunds == nil {
		l.refunds = map[string][]string{}
	}
	l.refunds[key] = entries // keyed like the ledger: a retry overwrites, never adds
	return nil
}

func (l *fakeLedger) Settle(_ context.Context, key string) (ledger.Settled, error) {
	if l.settleErr != nil {
		return ledger.Settled{}, l.settleErr
	}
	l.settled = append(l.settled, key)
	return ledger.Settled{LedgerTransactionID: "ltx-" + key, EntryIDs: []string{"e1-" + key, "e2-" + key}}, nil
}
func (l *fakeLedger) Release(_ context.Context, key string) error {
	l.released = append(l.released, key)
	return nil
}

type sentMsg struct {
	topic, key string
	value      []byte
}

type fakePublisher struct {
	msgs []sentMsg
	err  error
}

func (p *fakePublisher) Publish(_ context.Context, topic, key string, value []byte) error {
	if p.err != nil {
		return p.err
	}
	p.msgs = append(p.msgs, sentMsg{topic, key, value})
	return nil
}

// ---- rig -----------------------------------------------------------------------------------

type fakeReporter struct {
	calls []string
	err   error
}

func (r *fakeReporter) ReportNonceGap(_ context.Context, chainID int64, from uint64) error {
	r.calls = append(r.calls, fmt.Sprintf("%d/%d", chainID, from))
	return r.err
}

type rig struct {
	rep    *fakeReporter
	t      *testing.T
	st     *store.Store
	p      *Pipeline
	signer *fakeSigner
	chain  *fakeChain
	ledger *fakeLedger
	pub    *fakePublisher
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	r := &rig{t: t, st: st, signer: newSigner(), chain: newChain(), ledger: &fakeLedger{}, pub: &fakePublisher{}, rep: &fakeReporter{}}
	r.p = &Pipeline{Store: st, Ledger: r.ledger, Signer: r.signer, Chain: r.chain, Publisher: r.pub,
		ChainID: chainID, ConfirmationDepth: depth, Now: func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) },
		Gaps: &GapWatch{Store: st, Reporter: r.rep, Window: 256}}
	return r
}

// reserved creates a request and walks it to `reserved`, as the POST handler does.
func (r *rig) reserved(key string) store.Withdrawal {
	r.t.Helper()
	ctx := context.Background()
	w, _, err := r.st.Create(ctx, store.NewRequest{UserID: "alice", IdempotencyKey: key,
		AssetID: "11111111-1111-1111-1111-111111111111", Amount: big.NewInt(1_000_000_000_000_000), ToAddress: toAddr})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, step := range [][2]string{{store.StatusRequested, store.StatusValidated}, {store.StatusValidated, store.StatusReserved}} {
		if w, err = r.st.Transition(ctx, w.ID, step[0], step[1], nil); err != nil {
			r.t.Fatal(err)
		}
	}
	return w
}

func (r *rig) step() error { return r.p.Step(context.Background()) }

func (r *rig) get(key string) store.Withdrawal {
	r.t.Helper()
	w, err := r.st.Get(context.Background(), "alice", key)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// ---- tests ---------------------------------------------------------------------------------

func TestHappyPath(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k1")

	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got := r.get("k1")
	// One pass: signed and broadcast; the outbox event published, keyed on withdrawal_id.
	if got.Status != store.StatusBroadcast || got.Nonce == nil || *got.Nonce != 0 || got.TxHash == nil || got.SignedRawTx == nil || *got.ChainID != chainID {
		t.Fatalf("after one pass: %+v", got)
	}
	if len(r.chain.sent) != 1 || r.chain.sent[0] != *got.SignedRawTx {
		t.Fatalf("broadcast %d txs", len(r.chain.sent))
	}
	if h, _ := chain.TxHash(*got.SignedRawTx); strings.ToLower(h.Hex()) != *got.TxHash {
		t.Fatalf("tx_hash %s is not the hash of the stored tx", *got.TxHash)
	}
	if len(r.signer.keys) != 1 || r.signer.keys[0] != "withdrawal:"+w.ID {
		t.Fatalf("sign keys = %v, want only the Withdrawal Key", r.signer.keys)
	}
	if len(r.pub.msgs) != 1 || r.pub.msgs[0].topic != events.TopicWithdrawalsBroadcast || r.pub.msgs[0].key != w.ID {
		t.Fatalf("published = %+v", r.pub.msgs)
	}
	b, err := events.Decode[events.WithdrawalBroadcast](r.pub.msgs[0].value)
	if err != nil || b.WithdrawalID != w.ID || b.IdempotencyKey != "k1" || b.TxHash != *got.TxHash || b.Nonce != 0 || b.Amount != "1000000000000000" {
		t.Fatalf("broadcast event = %+v (%v)", b, err)
	}

	// Mined but not deep enough: nothing settles.
	r.chain.mine(true)
	r.chain.head += depth - 1
	_ = r.step()
	if r.get("k1").Status != store.StatusBroadcast || len(r.ledger.settled) != 0 {
		t.Fatal("settled one block before Withdrawal Confirmation Depth")
	}
	r.chain.head++
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got = r.get("k1")
	if got.Status != store.StatusConfirmed || *got.ConfirmedBlock != 100 || *got.SettleLedgerTxID != "ltx-withdrawal:"+w.ID {
		t.Fatalf("after depth: %+v", got)
	}
	if len(r.ledger.settled) != 1 || r.ledger.settled[0] != "withdrawal:"+w.ID || len(r.ledger.released) != 0 {
		t.Fatalf("settled %v released %v", r.ledger.settled, r.ledger.released)
	}
	if len(r.pub.msgs) != 2 || r.pub.msgs[1].topic != events.TopicWithdrawalsConfirmed || r.pub.msgs[1].key != w.ID {
		t.Fatalf("published = %+v", r.pub.msgs)
	}
	c, err := events.Decode[events.WithdrawalConfirmed](r.pub.msgs[1].value)
	if err != nil || c.WithdrawalID != w.ID || c.BlockNumber != r.chain.head {
		t.Fatalf("confirmed event = %+v (%v)", c, err)
	}
	// Steady state: further passes change and publish nothing.
	_ = r.step()
	_ = r.step()
	if len(r.ledger.settled) != 1 || len(r.pub.msgs) != 2 || len(r.chain.sent) != 1 {
		t.Fatal("repeated work after confirmation")
	}
}

// invariant #16: a lost sign response is retried with the same key and gets the same tx back,
// so exactly one nonce is ever allocated for the request.
func TestLostSignResponseNeverAllocatesASecondNonce(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	r.signer.loseReply = true
	_ = r.step()
	if r.get("k1").Status != store.StatusReserved || r.signer.next != 1 {
		t.Fatalf("after a lost reply: %s, nonces allocated %d", r.get("k1").Status, r.signer.next)
	}
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got := r.get("k1")
	if got.Status != store.StatusBroadcast || *got.Nonce != 0 || r.signer.next != 1 || len(r.ledger.released) != 0 {
		t.Fatalf("after retry: %s nonce %v, nonces allocated %d, released %v", got.Status, *got.Nonce, r.signer.next, r.ledger.released)
	}
}

// A transient wallet failure (e.g. 503 while a Nonce Gap is open) is retried, never released.
func TestTransientSignFailureIsRetried(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	r.signer.failNext = 2
	_ = r.step()
	_ = r.step()
	if r.get("k1").Status != store.StatusReserved || len(r.ledger.released) != 0 {
		t.Fatal("a transient failure released or moved the request")
	}
	if err := r.step(); err != nil || r.get("k1").Status != store.StatusBroadcast {
		t.Fatalf("third pass: %v %s", err, r.get("k1").Status)
	}
}

// wallet-service refused (no nonce allocated): Release, then failed. Nothing is broadcast.
func TestRefusedSignatureReleasesTheLock(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k1")
	r.signer.refuse = true
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got := r.get("k1")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonSignRefused || len(r.chain.sent) != 0 {
		t.Fatalf("%+v", got)
	}
	if len(r.ledger.released) != 1 || r.ledger.released[0] != "withdrawal:"+w.ID || len(r.ledger.settled) != 0 {
		t.Fatalf("released %v settled %v", r.ledger.released, r.ledger.settled)
	}
}

// A crash after the node accepted the tx but before the row said so: the re-send of the SAME tx is
// "already known" or already mined, and counts as sent.
func TestRebroadcastAfterACrashSendsTheSameTx(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	r.chain.broadcastErr = errors.New("rpc down")
	_ = r.step()
	got := r.get("k1")
	if got.Status != store.StatusSigned {
		t.Fatalf("status %s, want signed", got.Status)
	}
	raw := *got.SignedRawTx
	// Meanwhile the tx did land (the error was a lost response); the node now refuses it.
	r.chain.sent = []string{raw}
	r.chain.mine(true)
	r.chain.broadcastErr = errors.New("nonce too low")
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if got := r.get("k1"); got.Status != store.StatusBroadcast || *got.SignedRawTx != raw || r.signer.next != 1 {
		t.Fatalf("%s, raw changed %v, nonces %d", got.Status, *got.SignedRawTx != raw, r.signer.next)
	}
}

// Our nonce used by some other tx and ours not mined: never auto-resolved (invariant #4).
func TestNonceTakenByAnotherTxIsLeftForAnOperator(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	r.chain.broadcastErr = errors.New("nonce too low")
	if err := r.step(); err == nil {
		t.Fatal("expected an error")
	}
	if r.get("k1").Status != store.StatusSigned || len(r.ledger.released) != 0 {
		t.Fatal("moved or released a request whose tx may still matter")
	}
}

// Mined but reverted: nothing was paid, so the Lock is Released and the request fails.
func TestRevertedTxReleasesTheLock(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k1")
	_ = r.step()
	r.chain.mine(false)
	r.chain.revertReason = "execution reverted: ERC20: transfer amount exceeds balance"
	// Not deep yet: a shallow revert may still be reorged away, so nothing happens.
	r.chain.head += depth - 1
	_ = r.step()
	if r.get("k1").Status != store.StatusBroadcast || len(r.ledger.released) != 0 {
		t.Fatal("acted on a revert before Withdrawal Confirmation Depth")
	}
	r.chain.head++
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	got := r.get("k1")
	if got.Status != store.StatusFailed || *got.RejectReason != ReasonTxReverted || len(r.ledger.settled) != 0 ||
		len(r.ledger.released) != 1 || r.ledger.released[0] != "withdrawal:"+w.ID {
		t.Fatalf("%+v settled %v released %v", got, r.ledger.settled, r.ledger.released)
	}
}

// The tx paid out but the Lock was already resolved the other way: never confirmed silently.
func TestSettleConflictStaysBroadcast(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	r.ledger.settleErr = ledger.ErrConflict
	if err := r.step(); err == nil {
		t.Fatal("expected an error")
	}
	if r.get("k1").Status != store.StatusBroadcast || len(r.pub.msgs) != 1 {
		t.Fatalf("status %s, published %d", r.get("k1").Status, len(r.pub.msgs))
	}
}

// Kafka down: the state still advances, the events wait in the outbox and go out in order later.
func TestKafkaOutageLosesNothing(t *testing.T) {
	r := newRig(t)
	w := r.reserved("k1")
	r.pub.err = errors.New("kafka down")
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	_ = r.step()
	if r.get("k1").Status != store.StatusConfirmed || len(r.pub.msgs) != 0 {
		t.Fatal("state should advance while Kafka is down")
	}
	r.pub.err = nil
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	if len(r.pub.msgs) != 2 || r.pub.msgs[0].topic != events.TopicWithdrawalsBroadcast || r.pub.msgs[1].topic != events.TopicWithdrawalsConfirmed || r.pub.msgs[1].key != w.ID {
		t.Fatalf("published = %+v", r.pub.msgs)
	}
	_ = r.step()
	if len(r.pub.msgs) != 2 {
		t.Fatal("republished after acknowledgement")
	}
}

// Several requests at once: one nonce each, all confirm.
func TestSeveralRequests(t *testing.T) {
	r := newRig(t)
	for _, k := range []string{"a", "b", "c"} {
		r.reserved(k)
	}
	_ = r.step()
	r.chain.mine(true)
	r.chain.head += depth
	if err := r.step(); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, k := range []string{"a", "b", "c"} {
		got := r.get(k)
		if got.Status != store.StatusConfirmed || seen[*got.Nonce] {
			t.Fatalf("%s: %s nonce %d", k, got.Status, *got.Nonce)
		}
		seen[*got.Nonce] = true
	}
}

func TestHotWalletShort(t *testing.T) {
	for msg, want := range map[string]bool{
		"execution reverted: ERC20: transfer amount exceeds balance": true,
		"execution reverted: insufficient balance":                   true,
		"insufficient funds for gas * price + value":                 true,
		"execution reverted: Pausable: paused":                       false,
		"":                                                           false,
	} {
		if got := HotWalletShort(msg); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
}

// 409: the key already has a signature (a nonce) for another request — never released.
func TestKeyConflictIsNeverReleased(t *testing.T) {
	r := newRig(t)
	r.reserved("k1")
	r.signer.conflict = true
	if err := r.step(); err == nil {
		t.Fatal("expected an error")
	}
	if r.get("k1").Status != store.StatusReserved || len(r.ledger.released) != 0 {
		t.Fatalf("status %s, released %v", r.get("k1").Status, r.ledger.released)
	}
}
