package consumer

import (
	"context"
	"errors"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/ledger-service/internal/ledger"
	"digiwal/services/ledger-service/internal/screening"
	"digiwal/services/ledger-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	eth      = "11111111-1111-1111-1111-111111111111"
	sender   = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	badActor = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
	depAddr  = "0x90F79bf6EB2c4f870365E785982E1f101E93b906"
)

type fixedUsers map[string]string

func (f fixedUsers) UserForAddress(_ context.Context, _ int64, a string) (string, error) {
	if u, ok := f[a]; ok {
		return u, nil
	}
	return "", ErrUnknownAddress
}

type brokenUsers struct{ calls atomic.Int32 }

func (b *brokenUsers) UserForAddress(context.Context, int64, string) (string, error) {
	b.calls.Add(1)
	return "", errors.New("wallet-service unreachable")
}

func setup(t *testing.T, users UserResolver) (*ledger.Store, *Processor) {
	t.Helper()
	ctx := context.Background()
	s, err := ledger.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.SyncAssets(ctx, []ledger.Asset{{ID: eth, Type: ledger.AssetNative, Symbol: "ETH", Decimals: 18,
		HotFloor: big.NewInt(0), HotCeiling: big.NewInt(0), MinWithdrawalAmount: big.NewInt(1)}}); err != nil {
		t.Fatal(err)
	}
	return s, &Processor{Ledger: s, Screening: screening.New(badActor), Users: users}
}

func deposit(logIndex int64, from, amount string) events.DepositConfirmed {
	return events.DepositConfirmed{Version: events.Version, ChainID: 31337,
		TxHash:   "0x" + "ab" + "00000000000000000000000000000000000000000000000000000000000000"[:62],
		LogIndex: logIndex, AssetID: eth, FromAddress: from, ToAddress: depAddr,
		Amount: events.Amount(amount), BlockNumber: 100}
}

func msg(t *testing.T, e events.DepositConfirmed) Message {
	t.Helper()
	b, err := events.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return Message{Topic: events.TopicDepositsConfirmed, Key: []byte(e.Key()), Value: b}
}

func failedCount(t *testing.T, s *ledger.Store) (n int) {
	t.Helper()
	n, err := s.CountFailedEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDepositIsCreditedOnceEvenWhenRedelivered(t *testing.T) {
	s, p := setup(t, fixedUsers{depAddr: "alice"})
	ctx := context.Background()
	m := msg(t, deposit(-1, sender, "1000"))
	for i := 0; i < 3; i++ { // Kafka is at-least-once
		if err := p.Handle(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "1000" {
		t.Fatalf("balance %s, want 1000 (credited once)", b)
	}
	if c, _ := s.ChainBalance(ctx, eth); c.String() != "-1000" {
		t.Fatalf("chain balance %s", c)
	}
}

func TestErc20StyleLogsInOneTxAreSeparateDeposits(t *testing.T) {
	s, p := setup(t, fixedUsers{depAddr: "alice"})
	ctx := context.Background()
	for _, li := range []int64{0, 1} {
		if err := p.Handle(ctx, msg(t, deposit(li, sender, "10"))); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.String() != "20" {
		t.Fatalf("balance %s", b)
	}
}

// compliance-screening.md invariant #1
func TestDepositFromScreeningListIsFlaggedNotCredited(t *testing.T) {
	s, p := setup(t, fixedUsers{depAddr: "alice"})
	ctx := context.Background()
	m := msg(t, deposit(-1, badActor, "5000"))
	for i := 0; i < 2; i++ { // redelivery must not duplicate the flag (invariant #4)
		if err := p.Handle(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.Sign() != 0 {
		t.Fatalf("a flagged deposit credited %s", b)
	}
	if c, _ := s.ChainBalance(ctx, eth); c.Sign() != 0 {
		t.Fatalf("a flagged deposit moved the chain account: %s", c)
	}
	flagged, err := s.CountFlaggedDeposits(ctx)
	if err != nil || flagged != 1 {
		t.Fatalf("flagged=%d err=%v, want exactly 1 pending", flagged, err)
	}
	// case must not matter for the match
	lower := deposit(0, "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc", "1")
	if err := p.Handle(ctx, msg(t, lower)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.Sign() != 0 {
		t.Fatal("lower-cased blacklisted sender was credited")
	}
}

func TestUnprocessableEventsAreParkedAndAcknowledged(t *testing.T) {
	s, p := setup(t, fixedUsers{depAddr: "alice"})
	ctx := context.Background()

	unknownAsset := deposit(-1, sender, "10")
	unknownAsset.AssetID = "99999999-9999-9999-9999-999999999999"
	unknownAddr := deposit(0, sender, "10")
	unknownAddr.ToAddress = "0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65"

	cases := map[string]Message{
		"garbage":       {Topic: events.TopicDepositsConfirmed, Key: []byte("k"), Value: []byte(`{not json`)},
		"wrong version": {Topic: events.TopicDepositsConfirmed, Value: []byte(`{"version":2}`)},
		"zero amount":   msg(t, deposit(1, sender, "0")),
		"unknown asset": msg(t, unknownAsset),
		"unknown addr":  msg(t, unknownAddr),
	}
	for name, m := range cases {
		if err := p.Handle(ctx, m); err != nil {
			t.Errorf("%s: returned %v; an unprocessable event must be acknowledged, not retried forever", name, err)
		}
	}
	if got := failedCount(t, s); got != len(cases) {
		t.Fatalf("%d parked, want %d", got, len(cases))
	}
	if b, _ := s.Balance(ctx, "alice", eth); b.Sign() != 0 {
		t.Fatalf("balance %s: none of these may credit anyone", b)
	}
}

func TestTransientFailuresAreRetriedNotParked(t *testing.T) {
	s, p := setup(t, &brokenUsers{})
	if err := p.Handle(context.Background(), msg(t, deposit(-1, sender, "10"))); err == nil {
		t.Fatal("a resolver outage must surface as an error so the message is retried")
	}
	if got := failedCount(t, s); got != 0 {
		t.Fatalf("a transient failure was parked (%d)", got)
	}

	// no resolver wired at all (the open design gap): also retried, never silently dropped
	_, none := setup(t, nil)
	if err := none.Handle(context.Background(), msg(t, deposit(-1, sender, "10"))); err == nil {
		t.Fatal("missing UserResolver must not acknowledge the deposit")
	}
}

// ---- Run: at-least-once commit discipline ---------------------------------

type fakeReader struct {
	mu        sync.Mutex
	queue     []Message
	committed []Message
	done      chan struct{}
}

func (f *fakeReader) Fetch(ctx context.Context) (Message, error) {
	f.mu.Lock()
	if len(f.queue) > 0 {
		m := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return m, nil
	}
	f.mu.Unlock()
	<-ctx.Done()
	return Message{}, ctx.Err()
}

func (f *fakeReader) Commit(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, m)
	return nil
}

func TestRunCommitsOnlyAfterSuccessAndRetriesTransientFailures(t *testing.T) {
	s, p := setup(t, fixedUsers{depAddr: "alice"})
	// fail the first two attempts, then let the database through
	flaky := &flakyLedger{Ledger: s, failures: 2}
	p.Ledger = flaky

	r := &fakeReader{queue: []Message{msg(t, deposit(-1, sender, "100")), msg(t, deposit(0, sender, "50"))}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for { // stop once both records are committed
			r.mu.Lock()
			n := len(r.committed)
			r.mu.Unlock()
			if n == 2 {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := Run(ctx, r, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Balance(context.Background(), "alice", eth); b.String() != "150" {
		t.Fatalf("balance %s, want 150", b)
	}
	if flaky.attempts.Load() < 3 {
		t.Fatalf("only %d attempts: the failed record was not retried", flaky.attempts.Load())
	}
	if len(r.committed) != 2 {
		t.Fatalf("%d committed", len(r.committed))
	}
}

func TestRunDoesNotCommitARecordThatNeverSucceeds(t *testing.T) {
	_, p := setup(t, &brokenUsers{})
	r := &fakeReader{queue: []Message{msg(t, deposit(-1, sender, "1"))}}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := Run(ctx, r, p); err != nil {
		t.Fatal(err)
	}
	if len(r.committed) != 0 {
		t.Fatal("an unprocessed record was committed: the deposit would be lost")
	}
}

type flakyLedger struct {
	Ledger
	failures int32
	attempts atomic.Int32
}

func (f *flakyLedger) Apply(ctx context.Context, t ledger.Transaction) (ledger.Status, error) {
	if f.attempts.Add(1) <= f.failures {
		return "", errors.New("connection reset")
	}
	return f.Ledger.Apply(ctx, t)
}
