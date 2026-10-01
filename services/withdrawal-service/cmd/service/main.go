// Command withdrawal-service orchestrates withdrawals (docs/design/withdrawal-flow.md).
//
// Today (task 3.1): apply migrations, then serve /healthz on a plain listener and the
// withdrawal endpoints on an mTLS listener restricted to CN=api-gateway. A request is
// recorded idempotently and stays `requested`; validation, Reservation and signing are
// tasks 3.2-3.3. Without TLS certs the mTLS listener does not start.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"digiwal/services/withdrawal-service/internal/api"
	"digiwal/services/withdrawal-service/internal/authz"
	"digiwal/services/withdrawal-service/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	server := api.NewServer(st)

	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		tlsCfg, err := authz.ServerTLSConfig(certFile, requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("configuring mTLS: %v", err)
		}
		port := envOr("MTLS_PORT", "8443")
		srv := &http.Server{Addr: ":" + port, Handler: server.MTLSRoutes(), TLSConfig: tlsCfg}
		go func() {
			log.Printf("withdrawal-service mTLS listening on :%s", port)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	} else {
		log.Println("WARNING: TLS_CERT_FILE not set — mTLS listener disabled, withdrawal endpoints unavailable (run `make certs`)")
	}

	port := envOr("PORT", "8080")
	plain := &http.Server{Addr: ":" + port, Handler: server.Routes()}
	go func() {
		<-ctx.Done()
		_ = plain.Shutdown(context.Background())
	}()
	log.Printf("withdrawal-service listening on :%s", port)
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
