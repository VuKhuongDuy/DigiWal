package source

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Mongo is a read-only view of history-service's MongoDB. It exposes only find; the connecting user has
// the `read` role (roles/mongo.js).
type Mongo struct {
	client *mongo.Client
	db     *mongo.Database
}

func OpenMongo(ctx context.Context, uri, database string) (*Mongo, error) {
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo: %w", err)
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(ctx)
		return nil, fmt.Errorf("mongo: %w", err)
	}
	return &Mongo{client: c, db: c.Database(database)}, nil
}

func (m *Mongo) Close(ctx context.Context) { _ = m.client.Disconnect(ctx) }

// FindOne returns the document with the given _id, or mongo.ErrNoDocuments.
func (m *Mongo) FindOne(ctx context.Context, collection string, id any) (bson.M, error) {
	var doc bson.M
	err := m.db.Collection(collection).FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	return doc, err
}

// Find returns the documents matching filter.
func (m *Mongo) Find(ctx context.Context, collection string, filter bson.M) ([]bson.M, error) {
	cur, err := m.db.Collection(collection).Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	var out []bson.M
	return out, cur.All(ctx, &out)
}
