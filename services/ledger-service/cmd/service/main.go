// Command ledger-service is the source of truth for balances (docs/design/ledger.md).
//
// Startup: apply migrations, load and validate the canonical Asset Registry
// (infra/assets.yaml) and Screening List, sync the registry into Postgres, then serve
// /healthz on a plain listener and POST /v1/ledger/transactions on an mTLS listener
// restricted to CN=withdrawal-service. Without TLS certs the mTLS listener does not
// start, so nothing can post transactions.
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
	"digiwal/services/ledger-service/internal/api"
	"digiwal/services/ledger-service/internal/authz"
	"digiwal/services/ledger-service/internal/consumer"
	"digiwal/services/ledger-service/internal/ledger"
	"digiwal/services/ledger-service/internal/screening"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := ledger.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// Fail startup on any registry problem rather than boot half-usable.
	assets, err := ledger.LoadRegistry(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	if err := store.SyncAssets(ctx, assets); err != nil {
		log.Fatal(err)
	}
	log.Printf("asset registry loaded: %d assets", len(assets))

	screen, err := screening.Load(requireEnv("SCREENING_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("screening list loaded: %d addresses", screen.Len())

	server := api.NewServer(store)

	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		tlsCfg, err := authz.ServerTLSConfig(certFile, requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("configuring mTLS: %v", err)
		}
		port := envOr("MTLS_PORT", "8443")
		srv := &http.Server{Addr: ":" + port, Handler: server.MTLSRoutes(), TLSConfig: tlsCfg}
		go func() {
			log.Printf("ledger-service mTLS listening on :%s", port)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	} else {
		log.Println("WARNING: TLS_CERT_FILE not set — mTLS listener disabled, POST /v1/ledger/transactions unavailable (run `make certs`)")
	}

	// The deposits.confirmed consumer. It stays off without KAFKA_BROKERS so the service
	// (and the REST surface) still runs in an environment with no broker.
	if brokers := os.Getenv("KAFKA_BROKERS"); brokers != "" {
		reader := consumer.NewKafkaReader(strings.Split(brokers, ","), envOr("KAFKA_GROUP_ID", "ledger-service"), events.TopicDepositsConfirmed)
		defer reader.Close()
		proc := &consumer.Processor{Ledger: store, Screening: screen}
		go func() {
			if err := consumer.Run(ctx, reader, proc); err != nil {
				log.Fatalf("deposit consumer: %v", err)
			}
		}()
		log.Printf("deposit consumer running on %s (group %s)", events.TopicDepositsConfirmed, envOr("KAFKA_GROUP_ID", "ledger-service"))
	} else {
		log.Println("KAFKA_BROKERS not set: deposit consumer disabled")
	}

	port := envOr("PORT", "8080")
	plain := &http.Server{Addr: ":" + port, Handler: server.Routes()}
	go func() {
		<-ctx.Done()
		_ = plain.Shutdown(context.Background())
	}()
	log.Printf("ledger-service listening on :%s", port)
	if err := plain.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
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
