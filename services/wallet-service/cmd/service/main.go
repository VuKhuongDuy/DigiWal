// Command service is wallet-service. Modes:
//
//	service -init-seed hot_wallet  DEV profile only (a `!prod` build, SIGNER_BACKEND=software): generate the software Hot Wallet
//	                              seed, seal it with MASTER_KEY_HEX, store it, print the mnemonic once, exit. The user_derivation
//	                              seed is NOT created or read here any more: `key-admin init-seed user_derivation` makes it, and
//	                              only sweep-signer opens it (key-custody-hsm.md M3b).
//	service hot-wallet cancel --key <idempotency key>
//	                              operator only (withdrawal-flow.md invariant #23): sign and broadcast
//	                              a Nonce Filler — a 0-value Hot -> Hot tx at the key's nonce with
//	                              higher fees — so the signed withdrawal can never land. Run it
//	                              inside this container; there is no network endpoint for it.
//	service                       normal mode: serve the HTTP API.
//
// wallet-service holds NO user_derivation seed and has no endpoint that signs with a user key: Sweeps are signed by sweep-signer,
// and deposit addresses are issued from the Attested xpub (XPUB_ATTEST_PUBKEY_FILE, below), or not at all (503).
//
// SIGNER_BACKEND=software (default, DEV profile, `!prod` builds only) signs the Hot Wallet with the sealed hot_wallet seed, which is
// the only thing MASTER_KEY_HEX is still used for here. With SIGNER_BACKEND=pkcs11 the Hot Wallet is signed by hsm-gateway
// (HSM_GATEWAY_URL, mTLS) and MASTER_KEY_HEX is not needed; there is no software fallback. A `-tags prod` build contains no
// software signer, no seed code and no MASTER_KEY_HEX handling at all.
//
// XPUB_ATTEST_PUBKEY_FILE=<file> (key-custody-hsm.md M3a): new deposit addresses are derived by public derivation from wallet_xpubs after
// its attestation verifies, on every issuance, against the attestation public key in that file (kept outside the database; hex of
// the 65-byte P-256 point from `hsm-gateway init-token`, or a PEM). A swapped or tampered row latches issuance off with an alert
// (503 issuance_unavailable); stored addresses are still served. Unset = no new address can be issued (503): there is no
// seed-based fallback any more.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"digiwal/pkg/metrics"
	"digiwal/services/wallet-service/internal/api"
	"digiwal/services/wallet-service/internal/authz"
	"digiwal/services/wallet-service/internal/obs"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/treasury"
	"digiwal/services/wallet-service/internal/xpub"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hot-wallet" {
		runHotWallet(context.Background(), os.Args[2:])
		return
	}
	initSeed := flag.String("init-seed", "", "DEV profile only: generate + seal + store the software hot_wallet seed, print the mnemonic once, then exit")
	flag.Parse()

	ctx := context.Background()
	store, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if *initSeed != "" {
		runInitSeed(ctx, store, *initSeed)
		return
	}

	runServer(ctx, store)
}

// runHotWallet is the operator CLI: `hot-wallet cancel --key <key>` (invariant #23). The Hot key never
// leaves wallet-service: it signs the filler itself and broadcasts it. Running it again re-sends the
// same filler. Once it is mined, withdrawal-service's Reaper proves the nonce consumed and Releases.
func runHotWallet(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("hot-wallet cancel", flag.ExitOnError)
	key := fs.String("key", "", "the Hot Wallet idempotency key to cancel, e.g. withdrawal:<withdrawal_id>")
	if len(args) < 1 || args[0] != "cancel" {
		log.Fatal("usage: service hot-wallet cancel --key <idempotency key>")
	}
	_ = fs.Parse(args[1:])
	if *key == "" {
		log.Fatal("usage: service hot-wallet cancel --key <idempotency key>")
	}
	if err := signing.ProfileFromEnv(); err != nil {
		log.Fatalf("signer profile: %v", err)
	}
	store, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	hot, err := loadHotBackend(ctx, store)
	if err != nil {
		log.Fatalf("hot key backend: %v", err)
	}
	assets, err := signing.LoadAssets(requireEnv("ASSETS_FILE"))
	if err != nil {
		log.Fatalf("loading assets: %v", err)
	}
	rpc, err := signing.DialRPC(ctx, requireEnv("RPC_URL"))
	if err != nil {
		log.Fatal(err)
	}
	signer, err := signing.NewWithHot(ctx, store, hot, rpc, assets)
	if err != nil {
		log.Fatalf("hot wallet signer: %v", err)
	}
	adoptLegacyRows(ctx, store, signer)
	signer.WithFeeCeiling(feeCeiling())
	f, err := signer.Cancel(ctx, *key)
	if err != nil {
		var pr *signing.PolicyRefusedError
		if errors.As(err, &pr) {
			log.Fatalf("not cancelled: the Signing Policy refused the Nonce Filler (rule %s)", pr.Rule)
		}
		log.Fatalf("not cancelled: %v", err)
	}
	if err := rpc.Broadcast(ctx, f.SignedRawTx); err != nil {
		log.Fatalf("Nonce Filler %s stored (replacement %d) but not broadcast: %v — run the command again to re-send it", f.TxHash, f.Seq, err)
	}
	fmt.Printf("Nonce Filler broadcast: key %s, nonce %d, tx %s, max fee %s wei (replacement %d)\n", *key, f.Nonce, f.TxHash, f.MaxFeePerGas, f.Seq)
	fmt.Println("Once it is mined, the withdrawal-service Reaper proves nonce", f.Nonce, "consumed and Releases the Lock (failed / replaced_or_dropped).")
}

// feeCeiling is the hard cap on max fee per gas of any Fee Bump or Nonce Filler: Decided 50 gwei.
func feeCeiling() *big.Int {
	v := os.Getenv("HOT_WALLET_MAX_FEE_PER_GAS_WEI")
	if v == "" {
		return signing.DefaultFeeCeiling
	}
	n, ok := new(big.Int).SetString(v, 10)
	if !ok || n.Sign() <= 0 {
		log.Fatalf("HOT_WALLET_MAX_FEE_PER_GAS_WEI=%q is not a positive integer (wei)", v)
	}
	log.Printf("WARNING: HOT_WALLET_MAX_FEE_PER_GAS_WEI=%s overrides the decided default %s", n, signing.DefaultFeeCeiling)
	return n
}

func runServer(ctx context.Context, store *storage.Store) {
	if err := signing.ProfileFromEnv(); err != nil {
		log.Fatalf("signer profile: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: requireEnv("REDIS_ADDR"), Password: os.Getenv("REDIS_PASSWORD")})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connecting to redis: %v", err)
	}
	cache := storage.NewCache(rdb)

	// The Hot Wallet: the HSM key (pkcs11), or in a dev profile a separate software seed (wallet-tiering.md invariant #1) at BIP44
	// index 0. Signing needs the key, an EVM RPC (wallet-service builds the tx itself) and the asset list; without all three the
	// endpoint answers 503 instead of guessing.
	// The operator's Treasury file (Warm address per chain, wallet-tiering.md invariant #9).
	// Required: a missing or malformed file stops startup rather than serve a Warm address we
	// cannot trust.
	treasuryFile, err := treasury.Load(requireEnv("TREASURY_FILE"))
	if err != nil {
		log.Fatal(err)
	}

	var hotSigner api.HotSigner
	var system *api.SystemAddresses
	var hotKey string // keys the Hot nonce state (invariant 24)
	hotBackend, err := loadHotBackend(ctx, store)
	switch {
	case err == nil:
		signer := buildSigners(ctx, store, hotBackend)
		if signer != nil {
			hotSigner = signer
			hotKey = signer.HotKey()
			warm, err := treasuryFile.WarmFor(signer.ChainID(), signer.HotWalletAddress())
			if err != nil {
				log.Fatal(err)
			}
			cold, hasCold, err := treasuryFile.ColdFor(signer.ChainID(), signer.HotWalletAddress(), warm)
			if err != nil {
				log.Fatal(err)
			}
			system = &api.SystemAddresses{ChainID: signer.ChainID(), Hot: signer.HotWalletAddress(), Warm: warm, Cold: cold}
			if hasCold {
				log.Printf("treasury: chain %d, warm %s, cold %s", system.ChainID, warm.Hex(), cold.Hex())
			} else {
				log.Printf("treasury: chain %d, warm %s, no cold address declared", system.ChainID, warm.Hex())
			}
		}
	case errors.Is(err, storage.ErrNotFound):
		log.Println("WARNING: no hot_wallet seed found — run with -init-seed hot_wallet before Hot Wallet signing is used")
	default:
		// Under pkcs11 this is "hsm-gateway unreachable at start": stop, never fall back to a software key.
		log.Fatalf("loading the Hot Wallet signer: %v", err)
	}

	// Metrics: HTTP latency, the connection pool and the Go runtime, on their own internal port. Nothing key-related
	// is exported, and the signing listener never serves /metrics.
	reg := metrics.NewRegistry()
	reg.MustRegister(obs.Collector(nonceSource{store, hotKey}), metrics.PoolCollector("wallet-service", func() metrics.PoolStat { return metrics.PoolFromPgx(store.PoolStat()) }))
	if b, ok := hotBackend.(interface{ BreakerOpen() bool }); ok {
		// pkcs11 only: 1 while the Hot signer's circuit breaker is tripped (hsm-gateway failing), see alerts.yml HotSignerBreakerOpen.
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "digiwal_hot_signer_breaker_open",
			Help: "1 while the circuit breaker towards hsm-gateway is tripped (3 consecutive failures, no success since), else 0."},
			func() float64 {
				if b.BreakerOpen() {
					return 1
				}
				return 0
			}))
	}
	httpm := metrics.NewHTTP(reg, "wallet-service")
	go func() {
		addr := ":" + envOr("METRICS_PORT", metrics.DefaultPort)
		log.Printf("metrics on %s", addr)
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Printf("metrics listener: %v", err)
		}
	}()

	server := api.NewServer(store, cache, nil, hotSigner) // no seed-based Signer: issuance is xpub-only
	if pinFile := os.Getenv("XPUB_ATTEST_PUBKEY_FILE"); pinFile != "" {
		server.WithXPubIssuance(buildXPubIssuer(ctx, store, pinFile))
	}
	if system != nil {
		server.WithSystemAddresses(*system)
	}
	if hs, ok := hotSigner.(*signing.Signer); ok {
		server.WithHotKeys(hs)
	}

	// mTLS listener for /v1/sign/* (service-auth-mtls.md). Fail closed: without
	// certs it simply isn't started, so nothing can be signed.
	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		tlsCfg, err := authz.ServerTLSConfig(certFile, requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("configuring mTLS: %v", err)
		}
		mtlsPort := envOr("MTLS_PORT", "8443")
		srv := &http.Server{Addr: ":" + mtlsPort, Handler: httpm.Wrap(server.MTLSRoutes()), TLSConfig: tlsCfg}
		go func() {
			log.Printf("wallet-service mTLS listening on :%s", mtlsPort)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	} else {
		log.Println("WARNING: TLS_CERT_FILE not set — mTLS listener disabled, signing endpoints unavailable (run `make certs`)")
	}

	port := envOr("PORT", "8080")
	log.Printf("wallet-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, httpm.Wrap(server.Routes())))
}

// buildXPubIssuer loads the pinned attestation public key (a pin that cannot be read stops the service: verification is never
// skipped) and verifies the stored xpub once at startup. A bad row latches issuance off with an alert (the service keeps serving
// stored addresses); a missing row only means `key-admin migrate-xpub` has not run yet.
func buildXPubIssuer(ctx context.Context, store *storage.Store, pinFile string) *xpub.Issuer {
	pin, err := xpub.LoadPinned(pinFile)
	if err != nil {
		log.Fatalf("XPUB_ATTEST_PUBKEY_FILE: %v", err)
	}
	iss, err := xpub.NewIssuer(pin)
	if err != nil {
		log.Fatal(err)
	}
	switch row, err := store.XPub(ctx, "evm"); {
	case err == nil:
		if cerr := iss.Check(row.Record()); cerr != nil {
			log.Printf("ALERT the stored xpub does not verify against the pinned attestation key; new deposit addresses are refused until restart")
		} else {
			log.Printf("deposit addresses issue from the attested xpub (fingerprint %s, attestation key %.16s...), the seed is not used for issuance", xpub.Fingerprint(row.XPub), xpub.PinnedHex(pin))
		}
	case errors.Is(err, storage.ErrNotFound):
		log.Println("WARNING: XPUB_ATTEST_PUBKEY_FILE is set but wallet_xpubs is empty: new deposit addresses answer 503 until `key-admin migrate-xpub` has run")
	default:
		log.Fatalf("reading wallet_xpubs: %v", err)
	}
	return iss
}

// buildSigners wires the Hot Wallet signer. Returns nil (the endpoints answer 503) when RPC_URL / ASSETS_FILE aren't configured.
// There is no Sweep signer here any more: sweep-signer owns that.
func buildSigners(ctx context.Context, store *storage.Store, hot signing.HotBackend) *signing.Signer {
	rpcURL, assetsFile := os.Getenv("RPC_URL"), os.Getenv("ASSETS_FILE")
	if rpcURL == "" || assetsFile == "" {
		log.Println("WARNING: RPC_URL / ASSETS_FILE not set — signing endpoints disabled")
		return nil
	}
	assets, err := signing.LoadAssets(assetsFile)
	if err != nil {
		log.Fatalf("loading assets: %v", err)
	}
	chain, err := signing.DialRPC(ctx, rpcURL)
	if err != nil {
		log.Fatal(err)
	}
	hotSigner, err := signing.NewWithHot(ctx, store, hot, chain, assets)
	if err != nil {
		log.Fatalf("hot wallet signer: %v", err)
	}
	adoptLegacyRows(ctx, store, hotSigner)
	hotSigner.WithFeeCeiling(feeCeiling())
	log.Printf("hot_wallet signing enabled, address %s", hotSigner.HotWalletAddress())
	return hotSigner
}

// signerBackend is SIGNER_BACKEND (default software).
func signerBackend() string {
	if b := os.Getenv("SIGNER_BACKEND"); b != "" {
		return b
	}
	return signing.BackendSoftware
}

// loadHotBackend builds the Hot Wallet's key backend. pkcs11: an mTLS client to hsm-gateway, which
// pins the Hot address at start, and the hot_wallet seed is never read. software: the sealed
// hot_wallet seed at the Hot index (storage.ErrNotFound while it has not been initialised).
func loadHotBackend(ctx context.Context, store *storage.Store) (signing.HotBackend, error) {
	switch b := signerBackend(); b {
	case signing.BackendPKCS11:
		tlsCfg, err := signing.GatewayTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			return nil, fmt.Errorf("hsm-gateway client TLS: %w", err)
		}
		g, err := signing.NewGatewayHot(ctx, signing.GatewayConfig{
			BaseURL: requireEnv("HSM_GATEWAY_URL"), TLS: tlsCfg, CallerCN: "wallet-service", StartRetry: 60 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		log.Printf("Hot Key: hsm-gateway at %s, address %s, chain %d (the hot_wallet seed is not loaded)", os.Getenv("HSM_GATEWAY_URL"), mustAddr(g), g.PinnedChainID())
		return g, nil
	case signing.BackendSoftware:
		return loadSoftwareHot(ctx, store)
	default:
		return nil, fmt.Errorf("unknown SIGNER_BACKEND %q (want %q or %q)", b, signing.BackendSoftware, signing.BackendPKCS11)
	}
}

func mustAddr(h signing.HotBackend) string {
	a, _ := h.HotAddress(context.Background())
	return a.Hex()
}

// adoptLegacyRows attributes the unattributed (pre-migration-000005) Hot nonce state to the software
// Hot address. Only the software backend does it: a new HSM address must start from its own nonce
// (key-custody-hsm.md invariant 24) and the old rows stay with the old address until the cutover.
func adoptLegacyRows(ctx context.Context, store *storage.Store, s *signing.Signer) {
	if signerBackend() != signing.BackendSoftware {
		return
	}
	if err := store.AdoptLegacyHotAddress(ctx, s.HotKey()); err != nil {
		log.Fatalf("attributing legacy Hot Wallet nonce state to %s: %v", s.HotKey(), err)
	}
}

// nonceSource exports the nonce state of the current Hot address only.
type nonceSource struct {
	store *storage.Store
	hot   string
}

func (n nonceSource) NonceStates(ctx context.Context) ([]storage.NonceState, error) {
	return n.store.NonceStates(ctx, n.hot)
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
