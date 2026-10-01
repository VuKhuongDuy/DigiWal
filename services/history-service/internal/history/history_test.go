package history

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"digiwal/pkg/events"
)

type call struct {
	Op     string // progress | confirmed | failed
	Entry  Entry
	Status string
	Reason string
}

type fakeStore struct {
	mu    sync.Mutex
	calls []call
	fail  error // returned by every write while set
}

func (f *fakeStore) rec(c call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.calls = append(f.calls, c)
	return nil
}
func (f *fakeStore) RecordProgress(_ context.Context, e Entry, status string) error {
	return f.rec(call{Op: "progress", Entry: e, Status: status})
}
func (f *fakeStore) RecordConfirmed(_ context.Context, e Entry) error {
	return f.rec(call{Op: "confirmed", Entry: e})
}
func (f *fakeStore) RecordFailedEvent(_ context.Context, topic, key string, _ []byte, reason string) error {
	return f.rec(call{Op: "failed", Reason: reason, Entry: Entry{ID: topic + "/" + key}})
}

const (
	hash1  = "0xabababababababababababababababababababababababababababababababab"
	addrA  = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	addrB  = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
	asset  = "11111111-1111-1111-1111-111111111111"
	wdKey  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	bigAmt = "100000000000000000000" // 100 ETH in wei, beyond int64
)

func encode(t *testing.T, e events.Event) Message {
	t.Helper()
	b, err := events.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return Message{Topic: e.Topic(), Key: []byte(e.Key()), Value: b}
}

func TestDepositDetectedThenConfirmed(t *testing.T) {
	st := &fakeStore{}
	p := &Processor{Store: st}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	det := events.DepositDetected{Version: 1, ChainID: 11155111, TxHash: "0x" + strings.ToUpper(hash1[2:]), LogIndex: -1, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: bigAmt, BlockNumber: 7, DetectedAt: at}
	conf := events.DepositConfirmed{Version: 1, ChainID: 11155111, TxHash: hash1, LogIndex: -1, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: bigAmt, BlockNumber: 7}
	for _, m := range []Message{encode(t, det), encode(t, conf)} {
		if err := p.Handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.calls) != 2 {
		t.Fatalf("calls = %+v", st.calls)
	}
	d, c := st.calls[0], st.calls[1]
	if d.Op != "progress" || d.Status != StatusPending || c.Op != "confirmed" {
		t.Fatalf("ops = %s/%s status %s", d.Op, c.Op, d.Status)
	}
	// Both events are the same deposit: the same document.
	if d.Entry.ID != c.Entry.ID || d.Entry.ID != "deposit:11155111:"+hash1+":-1" {
		t.Fatalf("ids %q vs %q (tx hash case must not split a deposit in two)", d.Entry.ID, c.Entry.ID)
	}
	e := d.Entry
	if e.UserID != "alice" || e.Kind != KindDeposit || e.AssetID != asset || e.Amount != bigAmt || e.Address != addrB || e.BlockNumber != 7 || !e.At.Equal(at) {
		t.Fatalf("entry = %+v", e)
	}
}

func TestWithdrawalBroadcastThenConfirmed(t *testing.T) {
	st := &fakeStore{}
	p := &Processor{Store: st}
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	b := events.WithdrawalBroadcast{Version: 1, WithdrawalID: wdKey, IdempotencyKey: "client-key", UserID: "alice", AssetID: asset, Amount: "5", ToAddress: addrB,
		ChainID: 11155111, TxHash: hash1, Nonce: 3, BroadcastAt: at}
	c := events.WithdrawalConfirmed{Version: 1, WithdrawalID: wdKey, IdempotencyKey: "client-key", UserID: "alice", AssetID: asset, Amount: "5", ToAddress: addrB,
		ChainID: 11155111, TxHash: hash1, Nonce: 3, BlockNumber: 99, ConfirmedAt: at.Add(time.Hour)}
	for _, m := range []Message{encode(t, b), encode(t, c)} {
		if err := p.Handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if st.calls[0].Status != StatusBroadcast || st.calls[0].Op != "progress" || st.calls[1].Op != "confirmed" {
		t.Fatalf("calls = %+v", st.calls)
	}
	if st.calls[0].Entry.ID != "withdrawal:"+wdKey || st.calls[0].Entry.ID != st.calls[1].Entry.ID {
		t.Fatalf("ids = %q %q", st.calls[0].Entry.ID, st.calls[1].Entry.ID)
	}
	if st.calls[0].Entry.Kind != KindWithdrawal || st.calls[0].Entry.Address != addrB || st.calls[1].Entry.BlockNumber != 99 || !st.calls[1].Entry.At.Equal(at.Add(time.Hour)) {
		t.Fatalf("entries = %+v / %+v", st.calls[0].Entry, st.calls[1].Entry)
	}
}

// A deposit and a withdrawal must never share a document, whatever their keys look like.
func TestKindsNeverShareADocument(t *testing.T) {
	if depositEntry(1, hash1, 0, "u", asset, "1", addrA, 1, time.Now()).ID == withdrawalEntry("1:"+hash1+":0", "u", asset, "1", addrA, hash1, 1, 1, time.Now()).ID {
		t.Fatal("deposit and withdrawal ids collide")
	}
}

func TestUnprocessableEventsAreParkedAndAcknowledged(t *testing.T) {
	st := &fakeStore{}
	p := &Processor{Store: st}
	bad := []Message{
		{Topic: events.TopicDepositsConfirmed, Key: []byte("k"), Value: []byte(`{"version":1}`)},                               // missing fields
		{Topic: events.TopicDepositsDetected, Key: []byte("k"), Value: []byte(`not json`)},                                     // garbage
		{Topic: events.TopicWithdrawalsBroadcast, Key: []byte("k"), Value: []byte(`{"version":9}`)},                            // unknown schema version
		{Topic: "something.else", Key: []byte("k"), Value: []byte(`{}`)},                                                       // not ours
		{Topic: events.TopicWithdrawalsConfirmed, Key: []byte("k"), Value: []byte(`{"version":1,"user_id":"","amount":"-1"}`)}, // invalid
	}
	for _, m := range bad {
		if err := p.Handle(context.Background(), m); err != nil {
			t.Fatalf("%s: unprocessable must be acknowledged (nil), got %v", m.Topic, err)
		}
	}
	if len(st.calls) != len(bad) {
		t.Fatalf("calls = %d", len(st.calls))
	}
	for _, c := range st.calls {
		if c.Op != "failed" {
			t.Fatalf("unprocessable event reached the read model: %+v", c)
		}
	}
}

func TestStoreFailureIsTransientNotParked(t *testing.T) {
	st := &fakeStore{fail: errors.New("mongo down")}
	p := &Processor{Store: st}
	m := encode(t, events.DepositConfirmed{Version: 1, ChainID: 1, TxHash: hash1, LogIndex: 0, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: "1", BlockNumber: 1})
	if err := p.Handle(context.Background(), m); err == nil {
		t.Fatal("a store outage must surface as an error so the record is retried")
	}
	// Even a malformed event that cannot be parked (store down) must not be acknowledged.
	if err := p.Handle(context.Background(), Message{Topic: events.TopicDepositsConfirmed, Value: []byte("x")}); err == nil {
		t.Fatal("acknowledged an event it could not park")
	}
}

// ---- Run: at-least-once ----

type scriptedReader struct {
	mu        sync.Mutex
	msgs      []Message
	committed []Message
	done      chan struct{}
}

func (r *scriptedReader) Fetch(ctx context.Context) (Message, error) {
	r.mu.Lock()
	if len(r.msgs) > 0 {
		m := r.msgs[0]
		r.msgs = r.msgs[1:]
		r.mu.Unlock()
		return m, nil
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
	}
	return Message{}, ctx.Err()
}
func (r *scriptedReader) Commit(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committed = append(r.committed, m)
	return nil
}

func TestRunCommitsOnlyAfterSuccessAndRetriesTransientFailures(t *testing.T) {
	st := &fakeStore{fail: errors.New("mongo down")}
	msg := encode(t, events.DepositConfirmed{Version: 1, ChainID: 1, TxHash: hash1, LogIndex: 0, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: "1", BlockNumber: 1})
	rd := &scriptedReader{msgs: []Message{msg}, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, rd, &Processor{Store: st}, 10) }()

	time.Sleep(700 * time.Millisecond) // long enough for at least one failed attempt
	rd.mu.Lock()
	n := len(rd.committed)
	rd.mu.Unlock()
	if n != 0 {
		t.Fatal("committed the offset while the store was failing")
	}
	st.mu.Lock()
	st.fail = nil
	st.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rd.mu.Lock()
		n = len(rd.committed)
		rd.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 1 {
		t.Fatal("did not commit after the store recovered")
	}
	st.mu.Lock()
	applied := len(st.calls)
	st.mu.Unlock()
	if applied != 1 {
		t.Fatalf("applied %d times", applied)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestRunGivesUpAfterMaxAttemptsThenSkipsAndKeepsGoing(t *testing.T) {
	st := &fakeStore{fail: errors.New("mongo down")}
	first := encode(t, events.DepositConfirmed{Version: 1, ChainID: 1, TxHash: hash1, LogIndex: 0, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: "1", BlockNumber: 1})
	second := encode(t, events.DepositConfirmed{Version: 1, ChainID: 1, TxHash: hash1, LogIndex: 1, AssetID: asset,
		FromAddress: addrB, ToAddress: addrA, UserID: "alice", Amount: "2", BlockNumber: 1})
	rd := &scriptedReader{msgs: []Message{first, second}, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	fin := make(chan error, 1)
	go func() { fin <- Run(ctx, rd, &Processor{Store: st}, 2) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		rd.mu.Lock()
		n := len(rd.committed)
		rd.mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rd.mu.Lock()
	committed := len(rd.committed)
	rd.mu.Unlock()
	if committed < 1 {
		t.Fatal("never gave up on a record that kept failing: the partition is wedged")
	}
	// The store recovers: the next record is processed normally.
	st.mu.Lock()
	st.fail = nil
	st.mu.Unlock()
	for {
		rd.mu.Lock()
		n := len(rd.committed)
		rd.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-fin
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.calls) != 1 || st.calls[0].Entry.Amount != "2" {
		t.Fatalf("calls = %+v: want only the second record applied", st.calls)
	}
}
