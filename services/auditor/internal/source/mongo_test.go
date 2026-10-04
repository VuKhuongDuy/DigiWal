package source

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Needs a MongoDB with an admin user: TEST_MONGO_URI=mongodb://custody:custody@localhost:27017/?authSource=admin
// and roles/mongo.js already applied to it (mongosh ... --file roles/mongo.js).
func TestMongoReadOnlyUser(t *testing.T) {
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set TEST_MONGO_URI to run")
	}
	ctx := context.Background()
	admin, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Disconnect(ctx)
	hist := admin.Database("history").Collection("history")
	if _, err := hist.InsertOne(ctx, bson.M{"_id": "withdrawal:test-a2", "status": "confirmed"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = hist.DeleteOne(ctx, bson.M{"_id": "withdrawal:test-a2"}) })

	m, err := OpenMongo(ctx, strings.Replace(strings.Replace(uri, "custody:custody@", "auditor_ro:auditor_ro@", 1), "authSource=admin", "authSource=history", 1), "history")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(ctx)
	doc, err := m.FindOne(ctx, "history", "withdrawal:test-a2")
	if err != nil || doc["status"] != "confirmed" {
		t.Fatalf("read failed: %v %v", doc, err)
	}
	if _, err := m.db.Collection("history").InsertOne(ctx, bson.M{"_id": "x"}); err == nil {
		t.Error("auditor_ro could write to MongoDB")
	}
	if _, err := m.db.Collection("history").DeleteOne(ctx, bson.M{"_id": "withdrawal:test-a2"}); err == nil {
		t.Error("auditor_ro could delete from MongoDB")
	}
}
