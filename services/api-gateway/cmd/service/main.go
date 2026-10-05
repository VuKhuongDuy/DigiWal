// Command api-gateway is the only service reachable from outside the deployment
// (identity-auth.md invariant #5). It authenticates every request but POST /users with an
// API key and forwards only the user_id it resolved itself.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/api-gateway/internal/api"
	"digiwal/services/api-gateway/internal/obs"
	"digiwal/services/api-gateway/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// ledger-service and withdrawal-service are reached over mTLS with this service's own
	// certificate (CN=api-gateway).
	tlsCfg, err := api.ClientTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("configuring mTLS client: %v", err)
	}
	s := &api.Server{
		Auth: st, Users: st,
		Wallet: &api.WalletClient{BaseURL: requireEnv("WALLET_SERVICE_URL"), HTTP: &http.Client{Timeout: 10 * time.Second}},
		Ledger: &api.LedgerClient{BaseURL: requireEnv("LEDGER_SERVICE_URL"),
			HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}},
		// withdrawal-service, same mTLS client (CN=api-gateway).
		Withdrawals: &api.WithdrawalClient{BaseURL: requireEnv("WITHDRAWAL_SERVICE_URL"),
			HTTP: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}},
	}
	// Metrics: their own internal port, never the public listener (monitoring-dashboard.md invariant 6).
	reg := metrics.NewRegistry()
	reg.MustRegister(obs.Collector(st), metrics.PoolCollector("api-gateway", func() metrics.PoolStat { return metrics.PoolFromPgx(st.Pool().Stat()) }))
	httpm := metrics.NewHTTP(reg, "api-gateway")
	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()

	port := envOr("PORT", "8080")
	srv := &http.Server{Addr: ":" + port, Handler: httpm.Wrap(s.Routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	log.Printf("api-gateway listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
