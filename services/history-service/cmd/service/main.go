// Command history-service builds the transaction-history read model in MongoDB from the four
// Kafka topics it consumes (integration-contracts.md §1). Read model only: it never affects a
// balance and can be rebuilt by replaying the topics. Its query API (GET /v1/history) is a
// documented gap and is not built yet.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"digiwal/pkg/events"
	"digiwal/services/history-service/internal/history"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := history.ConnectMongo(ctx, requireEnv("MONGO_URI"), envOr("MONGO_DATABASE", "history"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close(context.Background())

	topics := []string{events.TopicDepositsDetected, events.TopicDepositsConfirmed, events.TopicWithdrawalsBroadcast, events.TopicWithdrawalsConfirmed}
	reader := history.NewKafkaReader(strings.Split(requireEnv("KAFKA_BROKERS"), ","), envOr("KAFKA_GROUP_ID", "history-service"), topics)
	defer reader.Close()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		log.Printf("healthz on :%s", envOr("PORT", "8080"))
		if err := http.ListenAndServe(":"+envOr("PORT", "8080"), mux); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("history-service consuming %s", strings.Join(topics, ", "))
	if err := history.Run(ctx, reader, &history.Processor{Store: store}); err != nil {
		log.Fatalf("consumer: %v", err)
	}
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
