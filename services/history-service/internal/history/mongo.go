package history

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Mongo stores the read model in two collections: "history" (one document per entry, _id =
// Entry.ID) and "failed_events".
type Mongo struct {
	client  *mongo.Client
	history *mongo.Collection
	failed  *mongo.Collection
}

func ConnectMongo(ctx context.Context, uri, database string) (*Mongo, error) {
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connecting to mongo: %w", err)
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(ctx)
		return nil, fmt.Errorf("pinging mongo: %w", err)
	}
	db := c.Database(database)
	m := &Mongo{client: c, history: db.Collection("history"), failed: db.Collection("failed_events")}
	// A user's feed, newest first.
	if _, err := m.history.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "updated_at", Value: -1}}}); err != nil {
		_ = c.Disconnect(ctx)
		return nil, fmt.Errorf("creating index: %w", err)
	}
	return m, nil
}

func (m *Mongo) Close(ctx context.Context) error { return m.client.Disconnect(ctx) }

// Collections exposes the underlying collections for tests.
func (m *Mongo) Collections() (history, failed *mongo.Collection) { return m.history, m.failed }

func fields(e Entry) bson.D {
	return bson.D{
		{Key: "user_id", Value: e.UserID}, {Key: "kind", Value: e.Kind}, {Key: "asset_id", Value: e.AssetID},
		{Key: "amount", Value: e.Amount}, {Key: "address", Value: e.Address}, {Key: "tx_hash", Value: e.TxHash},
		{Key: "chain_id", Value: e.ChainID},
	}
}

// RecordProgress is a pure $setOnInsert upsert: if the document exists in any status —
// including confirmed, because the events arrived out of order or were redelivered — it is
// left exactly as it is.
func (m *Mongo) RecordProgress(ctx context.Context, e Entry, status string) error {
	onInsert := append(fields(e),
		bson.E{Key: "status", Value: status}, bson.E{Key: "block_number", Value: e.BlockNumber},
		bson.E{Key: "created_at", Value: e.At}, bson.E{Key: "updated_at", Value: e.At})
	_, err := m.history.UpdateOne(ctx, bson.D{{Key: "_id", Value: e.ID}},
		bson.D{{Key: "$setOnInsert", Value: onInsert}}, options.UpdateOne().SetUpsert(true))
	return err
}

// RecordConfirmed sets the status to confirmed whether or not an earlier progress event was
// seen, so a lost or late one cannot hold a confirmed entry back. $set is idempotent.
func (m *Mongo) RecordConfirmed(ctx context.Context, e Entry) error {
	set := append(fields(e),
		bson.E{Key: "status", Value: StatusConfirmed}, bson.E{Key: "block_number", Value: e.BlockNumber},
		bson.E{Key: "confirmed_at", Value: e.At}, bson.E{Key: "updated_at", Value: e.At})
	_, err := m.history.UpdateOne(ctx, bson.D{{Key: "_id", Value: e.ID}},
		bson.D{
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: e.At}}},
		}, options.UpdateOne().SetUpsert(true))
	return err
}

func (m *Mongo) RecordFailedEvent(ctx context.Context, topic, key string, payload []byte, reason string) error {
	_, err := m.failed.InsertOne(ctx, bson.D{
		{Key: "topic", Value: topic}, {Key: "key", Value: key}, {Key: "payload", Value: string(payload)},
		{Key: "reason", Value: reason}, {Key: "failed_at", Value: time.Now().UTC()},
	})
	return err
}
