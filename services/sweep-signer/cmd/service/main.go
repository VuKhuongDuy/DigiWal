// Command service is sweep-signer (key-custody-hsm.md M3b, K10): the ONLY place the user_derivation Seed exists in plaintext.
// It signs Sweeps (a user's derived key -> the Hot Wallet) for sweeper and nothing else.
//
//	service              serve: mTLS :8443 (POST /v1/sign/user-address, CN sweeper, pinned leaf), plain :8080 (/healthz)
//	service healthcheck  exit 0 if the plain listener answers (the image has no shell and no curl)
//
// Environment (no secret is ever a value here: secrets are FILES under /run/secrets):
//
//	HSM_GATEWAY_URL        hsm-gateway: the Seed is opened by POST /v1/vault/unseal (KEK in the HSM), per request. No key of any kind is held here.
//	POSTGRES_DSN           role svc_sweepsigner, no password in it
//	POSTGRES_PASSWORD_FILE its password
//	TLS_CERT_FILE / TLS_KEY_FILE / TLS_CA_FILE   this service's leaf (CN sweep-signer) and the project CA
//	CALLERS_FILE           pinned leaf of sweeper (infra/pki/gen-callers.sh <file> sweeper)
//	RPC_URL, ASSETS_FILE   the EVM RPC and the Asset Registry
//	HOT_SOURCE             gateway (default; HSM_GATEWAY_URL, GET /v1/hot/key) | wallet-service (DEV builds only; WALLET_SERVICE_URL)
//	AUDIT_INTERVAL         Address Audit period (default 30s)
//	SWEEP_MAX_FEE_PER_GAS_WEI  the Fee Ceiling (default 50 gwei)
//	REQUIRE_MLOCK=1        refuse to start if the Seed page cannot be mlock'd
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/sweep-signer/internal/assets"
	"digiwal/services/sweep-signer/internal/audit"
	"digiwal/services/sweep-signer/internal/authz"
	"digiwal/services/sweep-signer/internal/hotkey"
	"digiwal/services/sweep-signer/internal/obs"
	"digiwal/services/sweep-signer/internal/seedpage"
	"digiwal/services/sweep-signer/internal/server"
	"digiwal/services/sweep-signer/internal/store"
	"digiwal/services/sweep-signer/internal/sweep"
	"digiwal/services/sweep-signer/internal/vault"
)

func main() {
	flag.Parse()
	if flag.Arg(0) == "healthcheck" {
		resp, err := http.Get("http://127.0.0.1:" + envOr("PORT", "8080") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	// First thing: no ptrace attach, no core file (invariant 16). Failure is logged, not fatal: a container that forbids
	// prctl is not made weaker by it.
	if err := seedpage.Harden(); err != nil {
		log.Printf("WARNING: process hardening (PR_SET_DUMPABLE=0, core limit 0) failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, requireEnv("POSTGRES_DSN"), os.Getenv("POSTGRES_PASSWORD_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	reg, err := assets.Load(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatalf("loading assets: %v", err)
	}
	chain, err := sweep.DialRPC(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}

	clientTLS, err := hotkey.ClientTLS(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
	if err != nil {
		log.Fatalf("client TLS: %v", err)
	}
	src := hotSource(clientTLS, chain)
	pinned, err := hotkey.PinWithRetry(ctx, src, 90*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Hot address pinned from %s: %s, chain %d (a Sweep can only ever go there; the request cannot change it)", src.Name(), pinned.Address, pinned.ChainID)

	seeds := &vault.Provider{Source: st, Unsealer: vault.NewGateway(requireEnv("HSM_GATEWAY_URL"), clientTLS), Strict: os.Getenv("REQUIRE_MLOCK") == "1"}
	// A page that cannot be locked is reported once at start, so the operator learns about it before a Sweep.
	if p, err := seedpage.New(seeds.Strict); err != nil {
		log.Fatalf("seed page: %v", err)
	} else {
		if !p.Locked() {
			log.Printf("WARNING: the Seed page cannot be mlock'd (RLIMIT_MEMLOCK too small or no CAP_IPC_LOCK): swap protection relies on the host having swap off")
		}
		p.Release()
	}

	auditor := audit.New(st, seeds)
	observer := obs.New(auditor, seeds)
	signer, err := sweep.New(ctx, st, seeds, chain, reg, pinned.Address, pinned.ChainID, auditor)
	if err != nil {
		log.Fatalf("sweep signer: %v", err)
	}
	signer.WithObserver(observer).WithFeeCeiling(feeCeiling())

	pins, err := authz.LoadPins(requireEnv("CALLERS_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	tlsCfg, err := authz.ServerTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"), pins)
	if err != nil {
		log.Fatalf("configuring mTLS: %v", err)
	}

	mreg := metrics.NewRegistry()
	mreg.MustRegister(observer)
	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, mreg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()

	interval := 30 * time.Second
	if s := os.Getenv("AUDIT_INTERVAL"); s != "" {
		if interval, err = time.ParseDuration(s); err != nil || interval <= 0 {
			log.Fatalf("AUDIT_INTERVAL=%q is not a positive duration", s)
		}
	}
	go auditor.Run(ctx, interval)

	srv := server.New(signer, pins)
	go func() {
		addr := ":" + envOr("MTLS_PORT", "8443")
		hs := &http.Server{Addr: addr, Handler: srv.MTLSRoutes(), TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
		log.Printf("sweep-signer mTLS listening on %s (CN sweeper, pinned leaf)", addr)
		log.Fatal(hs.ListenAndServeTLS("", ""))
	}()
	addr := ":" + envOr("PORT", "8080")
	log.Printf("sweep-signer listening on %s (/healthz only)", addr)
	log.Fatal((&http.Server{Addr: addr, Handler: srv.PlainRoutes(), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

// hotSource picks where the Hot address is pinned from. The default, and the only choice in a prod build, is hsm-gateway.
func hotSource(clientTLS *tls.Config, chain *sweep.RPCChain) hotkey.Source {
	switch s := envOr("HOT_SOURCE", "gateway"); s {
	case "gateway":
		return hotkey.NewGateway(requireEnv("HSM_GATEWAY_URL"), clientTLS)
	case "wallet-service":
		src, err := hotkey.NewWalletService(requireEnv("WALLET_SERVICE_URL"), clientTLS, func(ctx context.Context) (uint64, error) {
			id, err := chain.ChainID(ctx)
			if err != nil {
				return 0, err
			}
			return id.Uint64(), nil
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("WARNING: HOT_SOURCE=wallet-service (DEV profile): the Hot address is NOT taken from the HSM gateway")
		return src
	default:
		log.Fatalf("unknown HOT_SOURCE %q (want gateway or wallet-service)", s)
		return nil
	}
}

func feeCeiling() *big.Int {
	v := os.Getenv("SWEEP_MAX_FEE_PER_GAS_WEI")
	if v == "" {
		return sweep.DefaultFeeCeiling
	}
	n, ok := new(big.Int).SetString(strings.TrimSpace(v), 10)
	if !ok || n.Sign() <= 0 {
		log.Fatalf("SWEEP_MAX_FEE_PER_GAS_WEI=%q is not a positive integer (wei)", v)
	}
	log.Printf("WARNING: SWEEP_MAX_FEE_PER_GAS_WEI=%s overrides the default Fee Ceiling %s", n, sweep.DefaultFeeCeiling)
	return n
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
