// Command withdrawal-service orchestrates withdrawals (docs/design/withdrawal-flow.md).
//
// Today (tasks 3.1-3.6, 3.9, 3.10): apply migrations, load the Asset Registry and Screening
// List, learn the Hot/Warm addresses from wallet-service, then serve /healthz on a plain listener
// and the withdrawal and whitelist endpoints on an mTLS listener restricted to CN=api-gateway.
// A request is recorded idempotently, validated and Reserved in-line; a background pipeline
// signs, broadcasts, waits for Withdrawal Confirmation Depth and Settles it, publishing
// withdrawals.broadcast / withdrawals.confirmed. TLS certs are required: the service both serves
// and calls out over mTLS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"digiwal/services/withdrawal-service/internal/api"
	"digiwal/services/withdrawal-service/internal/authz"
	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/pipeline"
	"digiwal/services/withdrawal-service/internal/publisher"
	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/screening"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/validate"
	"digiwal/services/withdrawal-service/internal/wallet"
)

func main() {
	abandonID := flag.String("abandon", "", "operator action (withdrawal-flow.md invariant #9): abandon this disputed withdrawal_id — refund it if it had Settled, else Release it — once the chain proves its tx can never land; then exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *abandonID != "" {
		runAbandon(ctx, *abandonID)
		return
	}

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

	// wallet-service, over mTLS as CN=withdrawal-service: the Hot/Warm addresses, asked once here,
	// and the live deposit-address lookup (invariant #18). No request is served until the
	// addresses are known — the listeners start below this point.
	clientTLS, err := wallet.ClientTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("configuring mTLS client: %v", err)
	}
	walletClient := &wallet.Client{BaseURL: requireEnv("WALLET_SERVICE_URL"),
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS}}}
	system, err := fetchSystemAddresses(ctx, walletClient)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("system addresses: chain %d, hot %s, warm %s", system.ChainID, system.Hot.Hex(), system.Warm.Hex())
	destinations := &wallet.Destinations{System: system, Deposits: walletClient}

	// ledger-service, over mTLS as CN=withdrawal-service: Lock (in the request), Settle and Release.
	ledgerClient := &ledger.Client{BaseURL: requireEnv("LEDGER_SERVICE_URL"),
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS}}}

	// The chain we broadcast to must be the one wallet-service signs for.
	rpc, err := chain.Dial(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if id, err := rpc.ChainID(ctx); err != nil || id != system.ChainID {
		log.Fatalf("RPC chain %d (%v) is not wallet-service's chain %d", id, err, system.ChainID)
	}

	// Whitelist Cooldown is Decided at 24h (withdrawal-flow.md); override only for e2e tests.
	cooldown := envDuration("WHITELIST_COOLDOWN", 24*time.Hour)
	validator := &validate.Validator{Assets: assets, Whitelist: st, Screening: screen, Cooldown: cooldown, Destinations: destinations}
	server := api.NewServer(st, st, validator, api.Config{WhitelistCooldown: cooldown, Destinations: destinations, Reserver: ledgerClient})

	// Sign → broadcast → confirm → Settle, in the background (task 3.3).
	pub := publisher.NewKafka(strings.Split(requireEnv("KAFKA_BROKERS"), ","))
	defer pub.Close()
	pipe := &pipeline.Pipeline{Store: st, Ledger: ledgerClient, Signer: walletClient, Chain: rpc, Publisher: pub,
		ChainID: system.ChainID, ConfirmationDepth: envInt("WITHDRAWAL_CONFIRMATION_DEPTH", 64), // Decided: 64 on Sepolia
		// Nonce Gap detection (task 3.4): confirmed txs of the last NONCE_GAP_WINDOW blocks are re-checked.
		Gaps: &pipeline.GapWatch{Store: st, Reporter: walletClient, Window: envInt("NONCE_GAP_WINDOW", 256)}}
	go pipe.Run(ctx, envDuration("POLL_INTERVAL", 3*time.Second))
	log.Printf("withdrawal pipeline running: chain %d, confirmation depth %d", system.ChainID, pipe.ConfirmationDepth)

	// The Reaper (task 3.9, invariant #17). Decided (Tech Lead): 5 minutes, 5 minutes, 128 blocks.
	// Voider stays nil until task 3.13 builds wallet-service's /void: a stuck `reserved` row is
	// then only ALERTed, never Released.
	reaper := &pipeline.Reaper{Store: st, Ledger: ledgerClient, Chain: rpc, ChainID: system.ChainID,
		StuckRequestTimeout:     envDuration("STUCK_REQUEST_TIMEOUT", 5*time.Minute),
		StuckReservationTimeout: envDuration("STUCK_RESERVATION_TIMEOUT", 5*time.Minute),
		StuckBroadcastBlocks:    envInt("STUCK_BROADCAST_BLOCKS", 128)}
	go reaper.Run(ctx, envDuration("REAPER_INTERVAL", 30*time.Second))
	log.Printf("withdrawal reaper running: requested %s, reserved %s, broadcast %d blocks",
		reaper.StuckRequestTimeout, reaper.StuckReservationTimeout, reaper.StuckBroadcastBlocks)

	tlsCfg, err := authz.ServerTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("configuring mTLS: %v", err)
	}
	mtlsPort := envOr("MTLS_PORT", "8443")
	srv := &http.Server{Addr: ":" + mtlsPort, Handler: server.MTLSRoutes(), TLSConfig: tlsCfg}
	go func() {
		log.Printf("withdrawal-service mTLS listening on :%s", mtlsPort)
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}()

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

// runAbandon is the operator's way to resolve a disputed request (task 3.5); it serves nothing.
func runAbandon(ctx context.Context, id string) {
	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	clientTLS, err := wallet.ClientTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	lg := &ledger.Client{BaseURL: requireEnv("LEDGER_SERVICE_URL"),
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS}}}
	rpc, err := chain.Dial(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	w, err := pipeline.Abandon(ctx, st, lg, rpc, id)
	if err != nil {
		log.Fatalf("not abandoned: %v", err)
	}
	fmt.Printf("withdrawal %s is now %s (%s)\n", w.ID, w.Status, *w.RejectReason)
}

func fetchSystemAddresses(ctx context.Context, c *wallet.Client) (wallet.SystemAddresses, error) {
	var last error
	for i := 0; i < 30; i++ {
		a, err := c.SystemAddresses(ctx)
		if err == nil {
			return a, nil
		}
		last = err
		log.Printf("asking wallet-service for the system addresses (attempt %d/30): %v", i+1, err)
		select {
		case <-ctx.Done():
			return wallet.SystemAddresses{}, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	return wallet.SystemAddresses{}, fmt.Errorf("could not learn the Hot/Warm addresses from wallet-service: %w", last)
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
	if err != nil || d <= 0 {
		log.Fatalf("%s=%q is not a positive duration", key, v)
	}
	if d != def {
		log.Printf("WARNING: %s=%s overrides the default %s", key, d, def)
	}
	return d
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		log.Fatalf("%s=%q is not a positive integer", key, v)
	}
	if n != def {
		log.Printf("WARNING: %s=%d overrides the decided default %d", key, n, def)
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
