// Command sweeper moves confirmed deposits from users' attribution addresses to the Hot Wallet
// (docs/design/deposit-flow.md): a Gas Top-Up when an address cannot pay for an ERC-20 Sweep,
// then the Sweep. It holds no key material and no database: wallet-service signs, sweeper
// decides, broadcasts and waits, re-reading live chain state on every attempt.
package main

import (
	"context"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/sweeper/internal/chain"
	"digiwal/services/sweeper/internal/registry"
	"digiwal/services/sweeper/internal/screening"
	"digiwal/services/sweeper/internal/sweep"
	"digiwal/services/sweeper/internal/wallet"

	"github.com/ethereum/go-ethereum/common"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	assets, err := registry.LoadAssets(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	screen, err := screening.Load(requireEnv("SCREENING_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	hot := requireEnv("HOT_WALLET_ADDRESS")
	if !common.IsHexAddress(hot) {
		log.Fatalf("HOT_WALLET_ADDRESS %q is not an address", hot)
	}

	c, err := chain.Dial(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	chainID, err := c.ChainID(ctx)
	if err != nil {
		log.Fatalf("reading chain id: %v", err)
	}

	tlsCfg, err := wallet.ClientTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("configuring mTLS client: %v", err)
	}
	w := &wallet.Client{
		BaseURL: requireEnv("WALLET_SERVICE_URL"),
		HTTP:    &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}

	s := &sweep.Sweeper{
		Chain: c, Wallet: w, Assets: assets, Screening: screen,
		Config: sweep.Config{
			ChainID:   chainID,
			HotWallet: common.HexToAddress(hot),
			// deposit-flow.md: Gas Top-Up = 0.001 ETH; an address below 65,000 gas x 10 gwei cannot pay for a sweep.
			GasTopUp:         envBig("GAS_TOPUP_WEI", "1000000000000000"),
			GasReserve:       envBig("SWEEP_GAS_RESERVE_WEI", "650000000000000"),
			InclusionTimeout: envDuration("INCLUSION_TIMEOUT", 5*time.Minute),
			PollInterval:     envDuration("POLL_INTERVAL", 3*time.Second),
		},
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
		log.Printf("healthz on :%s", envOr("PORT", "8080"))
		if err := http.ListenAndServe(":"+envOr("PORT", "8080"), mux); err != nil {
			log.Fatal(err)
		}
	}()

	reader := sweep.NewKafkaReader(strings.Split(requireEnv("KAFKA_BROKERS"), ","), envOr("KAFKA_GROUP_ID", "sweeper"), events.TopicDepositsConfirmed)
	defer reader.Close()
	log.Printf("sweeper running: chain %d, hot wallet %s, consuming %s", chainID, hot, events.TopicDepositsConfirmed)
	if err := sweep.Run(ctx, reader, s, int(envInt("MAX_ATTEMPTS", 10))); err != nil {
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

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		log.Fatalf("%s=%q is not a positive integer", key, v)
	}
	return n
}

func envBig(key, def string) *big.Int {
	v := envOr(key, def)
	n, ok := new(big.Int).SetString(v, 10)
	if !ok || n.Sign() < 0 {
		log.Fatalf("%s=%q is not a non-negative integer", key, v)
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("%s=%q is not a duration", key, v)
	}
	return d
}
