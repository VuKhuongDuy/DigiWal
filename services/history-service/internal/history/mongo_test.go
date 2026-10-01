package history

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Runs only against a real MongoDB: TEST_MONGO_URI=mongodb://custody:custody@localhost:27017
// (the infra compose one). Skipped otherwise — the upsert semantics below are exactly what a
// fake cannot vouch for.
func mongoStore(t *testing.T) *Mongo {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("TEST_MONGO_URI not set")
	}
	ctx := context.Background()
	db := "history_test_" + time.Now().Format("150405.000000")
	m, err := ConnectMongo(ctx, uri, sanitize(db))
	if err != nil {
		t.Skipf("mongo unavailable: %v", err)
	}
	t.Cleanup(func() {
		h, _ := m.Collections()
		_ = h.Database().Drop(ctx)
		_ = m.Close(ctx)
	})
	return m
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == '.' {
			b[i] = '_'
		}
	}
	return string(b)
}

func doc(t *testing.T, m *Mongo, id string) bson.M {
	t.Helper()
	h, _ := m.Collections()
	var out bson.M
	if err := h.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, m *Mongo) int64 {
	t.Helper()
	h, _ := m.Collections()
	n, err := h.CountDocuments(context.Background(), bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func entry() Entry {
	return Entry{ID: "deposit:1:" + hash1 + ":-1", UserID: "alice", Kind: KindDeposit, AssetID: asset, Amount: bigAmt,
		Address: addrB, TxHash: hash1, ChainID: 1, BlockNumber: 5, At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

func TestMongoPendingThenConfirmed(t *testing.T) {
	m, ctx := mongoStore(t), context.Background()
	e := entry()
	if err := m.RecordProgress(ctx, e, StatusPending); err != nil {
		t.Fatal(err)
	}
	if d := doc(t, m, e.ID); d["status"] != StatusPending || d["amount"] != bigAmt {
		t.Fatalf("doc = %v", d)
	}
	e.At = e.At.Add(time.Hour)
	if err := m.RecordConfirmed(ctx, e); err != nil {
		t.Fatal(err)
	}
	d := doc(t, m, e.ID)
	if d["status"] != StatusConfirmed || d["confirmed_at"] == nil || count(t, m) != 1 {
		t.Fatalf("doc = %v (count %d)", d, count(t, m))
	}
	// created_at keeps the original detection time.
	if got := d["created_at"].(bson.DateTime).Time().UTC(); !got.Equal(entry().At) {
		t.Fatalf("created_at = %v", got)
	}
}

func TestMongoOutOfOrderAndRedeliveryNeverRegress(t *testing.T) {
	m, ctx := mongoStore(t), context.Background()
	e := entry()
	// confirmed arrives first (detected was lost or is late), then detected, then both again.
	for i := 0; i < 2; i++ {
		if err := m.RecordConfirmed(ctx, e); err != nil {
			t.Fatal(err)
		}
		if err := m.RecordProgress(ctx, e, StatusPending); err != nil {
			t.Fatal(err)
		}
	}
	if d := doc(t, m, e.ID); d["status"] != StatusConfirmed || count(t, m) != 1 {
		t.Fatalf("a late/duplicate progress event regressed the entry: %v", d)
	}
}

// failed is terminal and may follow confirmed (a Settled withdrawal reorged out and refunded); an
// older event redelivered afterwards must not bring the entry back, and a failure without a tx must
// not erase the tx hash the entry already has.
func TestMongoFailedIsTerminalAndMayFollowConfirmed(t *testing.T) {
	m := mongoStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	e := withdrawalEntry("w1", "alice", "asset", "100", "0xdest", "0xtx", 1, 50, at)

	if err := m.RecordProgress(ctx, e, StatusBroadcast); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordConfirmed(ctx, e); err != nil {
		t.Fatal(err)
	}
	failed := e
	failed.TxHash, failed.FailReason = "", "abandoned_refunded"
	failed.At = at.Add(time.Hour)
	if err := m.RecordFailed(ctx, failed); err != nil {
		t.Fatal(err)
	}
	d := doc(t, m, e.ID)
	if d["status"] != StatusFailed || d["fail_reason"] != "abandoned_refunded" || d["tx_hash"] != "0xtx" {
		t.Fatalf("after confirmed → failed: %v", d)
	}
	// Redelivered (or late) older events change nothing.
	if err := m.RecordConfirmed(ctx, e); err != nil {
		t.Fatalf("a redelivered confirmed after failed must be a no-op, not an error: %v", err)
	}
	if err := m.RecordProgress(ctx, e, StatusBroadcast); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordFailed(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if d := doc(t, m, e.ID); d["status"] != StatusFailed || count(t, m) != 1 {
		t.Fatalf("failed was undone: %v", d)
	}

	// Failure first (never broadcast): inserts a failed entry; a late broadcast cannot undo it.
	never := withdrawalEntry("w2", "bob", "asset", "5", "0xdest", "", 1, 0, at)
	never.FailReason = "stale_request"
	if err := m.RecordFailed(ctx, never); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordProgress(ctx, never, StatusBroadcast); err != nil {
		t.Fatal(err)
	}
	if d := doc(t, m, never.ID); d["status"] != StatusFailed || d["tx_hash"] != "" {
		t.Fatalf("%v", d)
	}
}
