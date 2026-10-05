// Command chain-watcher is the Deposit Detection service (docs/design/deposit-flow.md): it
// watches the EVM chain for transfers to addresses wallet-service issued, waits for
// Confirmation Depth, and publishes deposits.detected / deposits.confirmed. It only reads the
// chain — it never signs or broadcasts.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/chain-watcher/internal/chain"
	"digiwal/services/chain-watcher/internal/obs"
	"digiwal/services/chain-watcher/internal/publisher"
	"digiwal/services/chain-watcher/internal/registry"
	"digiwal/services/chain-watcher/internal/resolver"
	"digiwal/services/chain-watcher/internal/store"
	"digiwal/services/chain-watcher/internal/watcher"

	"github.com/ethereum/go-ethereum/common"
	"github.com/redis/go-redis/v9"
)

// advisoryLockKey serializes chain-watcher instances: two would each scan and publish.
const advisoryLockKey = 0x63687769 // "chwi"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	assetsFile := requireEnv("ASSETS_FILE")
	if _, err := registry.Load(assetsFile); err != nil { // fail startup on a bad registry
		log.Fatal(err)
	}

	c, err := chain.Dial(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	chainID, _ := c.ChainID(ctx)

	tlsCfg, err := resolver.ClientTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("configuring mTLS client: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: envOr("REDIS_ADDR", "localhost:6379")})
	defer rdb.Close()
	res := resolver.New(rdb,
		&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		requireEnv("WALLET_SERVICE_URL"), envDuration("NEGATIVE_CACHE_TTL", 30*time.Second))

	// Hot Wallet Outflow (Gas Top-Ups) must never be recorded as a deposit (deposit-flow.md
	// invariant #17), so the address is needed before the first block is scanned. wallet-service
	// may still be starting: retry for a while, then exit non-zero.
	walletHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	hot, err := fetchHotAddress(ctx, walletHTTP, requireEnv("WALLET_SERVICE_URL"))
	if err != nil {
		log.Fatal(err)
	}

	pub := publisher.NewKafka(strings.Split(requireEnv("KAFKA_BROKERS"), ","))
	defer pub.Close()

	cfg := watcher.Config{
		ConfirmationDepth: envInt("CONFIRMATION_DEPTH", 32), // Sepolia, deposit-flow.md; use 1-2 on Anvil
		BatchBlocks:       envInt("BATCH_BLOCKS", 100),
	}
	if v := os.Getenv("START_BLOCK"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			log.Fatalf("START_BLOCK %q is not a block number", v)
		}
		cfg.StartBlock = &n
	}
	w := &watcher.Watcher{
		Chain: c, Store: st, Resolver: res, Publisher: pub, Config: cfg, HotWallet: hot,
		LoadRegistry: func() (*registry.Registry, error) { return registry.Load(assetsFile) },
	}

	// Metrics on their own internal port: the connection pool, the Go runtime, and the lag figures (watermark, head on its
	// own ticker, confirmation depth).
	reg := metrics.NewRegistry()
	heads := &obs.HeadTicker{}
	reg.MustRegister(metrics.PoolCollector("chain-watcher", func() metrics.PoolStat { return metrics.PoolFromPgx(st.Pool().Stat()) }),
		obs.Collector(st, heads, chainID, envInt("CONFIRMATION_DEPTH", 32)))
	go heads.Run(ctx, c, 5*time.Second) // the head on its own ticker, independent of the poll loop
	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()

	// /healthz on its own plain listener; chain-watcher has no mTLS server side — it only calls out.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusOK) })
		log.Printf("healthz on :%s", envOr("PORT", "8080"))
		if err := http.ListenAndServe(":"+envOr("PORT", "8080"), mux); err != nil {
			log.Fatal(err)
		}
	}()

	// One active instance: hold a session advisory lock on a dedicated connection.
	lockConn, err := st.Pool().Acquire(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer lockConn.Release()
	for {
		var got bool
		if err := lockConn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&got); err != nil {
			log.Fatal(err)
		}
		if got {
			break
		}
		log.Println("another chain-watcher instance is active; waiting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}

	log.Printf("chain-watcher running: chain %d, confirmation depth %d, hot wallet %s", chainID, cfg.ConfirmationDepth, hot.Hex())
	w.Run(ctx, envDuration("POLL_INTERVAL", 6*time.Second))
}

func fetchHotAddress(ctx context.Context, c *http.Client, walletURL string) (common.Address, error) {
	var last error
	for i := 0; i < 30; i++ {
		addr, err := resolver.FetchHotAddress(ctx, c, walletURL)
		if err == nil {
			return addr, nil
		}
		last = err
		log.Printf("asking wallet-service for the hot wallet address (attempt %d/30): %v", i+1, err)
		select {
		case <-ctx.Done():
			return common.Address{}, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	return common.Address{}, fmt.Errorf("could not learn the hot wallet address from wallet-service: %w", last)
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
	if err != nil || n < 0 {
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
