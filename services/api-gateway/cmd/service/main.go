// Command api-gateway is the only service reachable from outside the deployment
// (identity-auth.md invariant #5). It authenticates every request but POST /users with an
// API key and forwards only the user_id it resolved itself.
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/api-gateway/internal/api"
	"digiwal/services/api-gateway/internal/obs"
	"digiwal/services/api-gateway/internal/store"
	"github.com/redis/go-redis/v9"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.ConnectWithPool(ctx, requireEnv("POSTGRES_DSN"), int32(envPositiveInt("POSTGRES_MAX_CONNS", 0)))
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
	reg := metrics.NewRegistry()
	upstream := metrics.NewUpstream(reg, "api-gateway") // client-side time of each call to another service
	// LEDGER_SERVICE_ENDPOINTS (host:port,host:port) makes the gateway spread its calls over several ledger-services itself, per request (api.Balancer);
	// LEDGER_SERVICE_URL then supplies the name the certificates are checked against. Without it the single URL is used.
	var ledgerRT http.RoundTripper = pooledTransport(tlsCfg)
	if eps := os.Getenv("LEDGER_SERVICE_ENDPOINTS"); eps != "" {
		lu, err := url.Parse(requireEnv("LEDGER_SERVICE_URL"))
		if err != nil || lu.Hostname() == "" {
			log.Fatalf("LEDGER_SERVICE_URL is not a URL: %v", err)
		}
		ledgerRT = api.NewBalancer(strings.Split(eps, ","), func() http.RoundTripper {
			c := tlsCfg.Clone()
			c.ServerName = lu.Hostname() // the instances are addressed by IP; their certificate says ledger-service
			return pooledTransport(c)
		}, 5*time.Second)
	}
	s := &api.Server{
		Auth: st, Users: st,
		Wallet: &api.WalletClient{BaseURL: requireEnv("WALLET_SERVICE_URL"),
			HTTP: &http.Client{Timeout: 10 * time.Second, Transport: upstream.Transport("wallet", pooledTransport(nil))}},
		Ledger: &api.LedgerClient{BaseURL: requireEnv("LEDGER_SERVICE_URL"),
			HTTP: &http.Client{Timeout: 10 * time.Second, Transport: upstream.Transport("ledger", ledgerRT)}},
		// withdrawal-service, same mTLS client (CN=api-gateway).
		Withdrawals: &api.WithdrawalClient{BaseURL: requireEnv("WITHDRAWAL_SERVICE_URL"),
			HTTP: &http.Client{Timeout: 15 * time.Second, Transport: upstream.Transport("withdrawal", pooledTransport(tlsCfg))}},
	}
	// Rate limits, requests per minute; 0 disables. In-memory and per instance by default; with
	// REDIS_ADDR the budget is shared across instances, falling back to the in-memory limiter
	// (fail-open) whenever Redis errors.
	var rdb *redis.Client
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		// One time budget for dial, read, write and the wait for a pooled connection (REDIS_TIMEOUT_MS, default 100): at ~3,000 req/s on a CPU-bound
		// node 100 ms was exceeded by scheduling alone, and every timeout closed a connection that the next call had to dial (and resolve) again.
		// REDIS_MIN_IDLE_CONNS keeps that many connections open in the pool so that a burst does not have to dial them.
		redisTimeout := time.Duration(envInt("REDIS_TIMEOUT_MS", 100)) * time.Millisecond
		rdb = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("REDIS_PASSWORD"), MaxRetries: -1,
			DialTimeout: redisTimeout, ReadTimeout: redisTimeout, WriteTimeout: redisTimeout, MinIdleConns: envInt("REDIS_MIN_IDLE_CONNS", 0),
			// go-redis defaults to 10 connections per CPU and waits ReadTimeout+1s for a free one: at ~2,000 req/s with 100+ ms round trips
			// that is hundreds of calls in flight, and the cache (and the rate limiter) errored out. A big pool, and a short wait for it.
			PoolSize: envInt("REDIS_POOL_SIZE", 200), PoolTimeout: redisTimeout})
		defer rdb.Close()
	}
	newLimiter := func(prefix string, perMin int) api.RateLimiter {
		if perMin <= 0 {
			return nil
		}
		local := api.NewLimiter(perMin, time.Minute)
		if rdb == nil {
			return local
		}
		return &api.RedisLimiter{Client: rdb, Prefix: "ratelimit:" + prefix + ":", Limit: perMin, Window: time.Minute, Fallback: local}
	}
	// Read cache for the API-key lookup (docs/design/read-cache.md), off unless AUTH_CACHE_ENABLED=1 and Redis is configured.
	authStats := metrics.NewCache("api-gateway", "apikey")
	if rdb != nil && os.Getenv("AUTH_CACHE_ENABLED") == "1" {
		s.Auth = &api.AuthCache{Next: st, Revoker: st, KV: api.RedisKV{C: rdb}, TTL: time.Duration(envInt("AUTH_CACHE_TTL_SECONDS", 300)) * time.Second, Stats: authStats}
		log.Printf("api-key cache on (ttl %ds)", envInt("AUTH_CACHE_TTL_SECONDS", 300))
	}
	s.IPLimit = newLimiter("ip", envInt("RATE_LIMIT_IP_PER_MIN", 1200))
	s.UserLimit = newLimiter("user", envInt("RATE_LIMIT_USER_PER_MIN", 600))
	// Metrics: their own internal port, never the public listener (monitoring-dashboard.md invariant 6).
	reg.MustRegister(metrics.CacheCollector(authStats))
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

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Fatalf("env var %s must be a non-negative integer, got %q", key, v)
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pooledTransport is an HTTP transport that keeps connections for reuse. net/http keeps only two idle connections
// per host by default, so under load nearly every call to wallet-service, ledger-service or withdrawal-service
// opened a new TCP connection (and a TLS handshake for the mTLS ones): measured at 800 requests/s, ~1,750
// sockets in TIME_WAIT toward wallet-service's port and the gateway pinned at ~1.7 cores. tlsCfg is nil for plain HTTP.
func pooledTransport(tlsCfg *tls.Config) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tlsCfg
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 128
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// envPositiveInt reads a positive integer from the environment, or def when unset or not a positive integer
// (envInt above is the strict one: it refuses an invalid value, and 0 is allowed).
func envPositiveInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}
