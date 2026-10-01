// Command withdrawal-service orchestrates withdrawals (docs/design/withdrawal-flow.md).
//
// Today (tasks 3.1, 3.2, 3.6): apply migrations, load the Asset Registry and Screening List,
// then serve /healthz on a plain listener and the withdrawal and whitelist endpoints on an
// mTLS listener restricted to CN=api-gateway. A request is recorded idempotently, then
// validated in-line (`validated` or `rejected`); Reservation and signing are task 3.3.
// Without TLS certs the mTLS listener does not start.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"digiwal/services/withdrawal-service/internal/api"
	"digiwal/services/withdrawal-service/internal/authz"
	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/screening"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/validate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// Fail startup on a bad registry or screening list rather than validate against a half one.
	assets, err := registry.Load(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	screen, err := screening.Load(requireEnv("SCREENING_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("asset registry loaded: %d assets; screening list: %d addresses", assets.Len(), screen.Len())

	// Whitelist Cooldown is Decided at 24h (withdrawal-flow.md); override only for e2e tests.
	cooldown := envDuration("WHITELIST_COOLDOWN", 24*time.Hour)
	validator := &validate.Validator{Assets: assets, Whitelist: st, Screening: screen, Cooldown: cooldown}
	server := api.NewServer(st, st, validator, api.Config{WhitelistCooldown: cooldown})

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

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Fatalf("%s=%q is not a duration", key, v)
	}
	if d != def {
		log.Printf("WARNING: %s=%s overrides the decided default %s", key, d, def)
	}
	return d
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
