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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"digiwal/pkg/events"
	"digiwal/pkg/metrics"
	"digiwal/services/ledger-service/internal/api"
	"digiwal/services/ledger-service/internal/authz"
	"digiwal/services/ledger-service/internal/consumer"
	"digiwal/services/ledger-service/internal/ledger"
	"digiwal/services/ledger-service/internal/obs"
	"digiwal/services/ledger-service/internal/screening"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Metrics (monitoring-dashboard.md decision (d)): one registry; both API listeners are measured, and neither serves it.
	reg := metrics.NewRegistry()
	store, err := ledger.ConnectWithPoolMetrics(ctx, requireEnv("POSTGRES_DSN"), int32(envInt("POSTGRES_MAX_CONNS", 0)), metrics.NewDB(reg, "ledger-service"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	reg.MustRegister(obs.Collector(store), metrics.PoolCollector("ledger-service", func() metrics.PoolStat { return metrics.PoolFromPgx(store.PoolStat()) }))
	httpm := metrics.NewHTTP(reg, "ledger-service")

	// Balance read cache (docs/design/read-cache.md): off unless BALANCE_CACHE_ENABLED=1 and REDIS_ADDR is set. Redis trouble never fails a request.
	var balCache *api.BalanceCache
	if addr := os.Getenv("REDIS_ADDR"); addr != "" && os.Getenv("BALANCE_CACHE_ENABLED") == "1" {
		// REDIS_TIMEOUT_MS and REDIS_MIN_IDLE_CONNS: see api-gateway.
		redisTimeout := time.Duration(envInt("REDIS_TIMEOUT_MS", 100)) * time.Millisecond
		rdb := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("REDIS_PASSWORD"), MaxRetries: -1,
			DialTimeout: redisTimeout, ReadTimeout: redisTimeout, WriteTimeout: redisTimeout, MinIdleConns: envInt("REDIS_MIN_IDLE_CONNS", 0),
			// see api-gateway: the default pool (10 per CPU) ran dry at ~2,000 req/s and every miss showed up as a cache error
			PoolSize: envInt("REDIS_POOL_SIZE", 200), PoolTimeout: redisTimeout})
		defer rdb.Close()
		stats := metrics.NewCache("ledger-service", "balances")
		reg.MustRegister(metrics.CacheCollector(stats))
		balCache = &api.BalanceCache{KV: api.RedisKV{C: rdb}, TTL: time.Duration(envInt("BALANCE_CACHE_TTL_SECONDS", 5)) * time.Second, Stats: stats}
		store.OnBalanceChange(balCache.Invalidate)
		log.Printf("balance cache on (ttl %ds)", envInt("BALANCE_CACHE_TTL_SECONDS", 5))
	}

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
	if balCache != nil {
		server.WithBalanceCache(balCache)
	}

	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		tlsCfg, err := authz.ServerTLSConfig(certFile, requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("configuring mTLS: %v", err)
		}
		port := envOr("MTLS_PORT", "8443")
		srv := &http.Server{Addr: ":" + port, Handler: httpm.Wrap(server.MTLSRoutes()), TLSConfig: tlsCfg}
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

	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()

	port := envOr("PORT", "8080")
	plain := &http.Server{Addr: ":" + port, Handler: httpm.Wrap(server.Routes())}
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

// envInt reads a positive integer from the environment, or def when unset or not a positive integer.
func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}
