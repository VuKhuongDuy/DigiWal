// Command service is wallet-service. Two modes:
//
//	service -init-seed <role>     generate the seed for <role> (user_derivation | hot_wallet),
//	                              seal it, store it, print the mnemonic once, exit. Run this
//	                              exactly once per role, ever, per environment.
//	service                       normal mode: decrypt the sealed seeds already in Postgres
//	                              (using MASTER_KEY_HEX) and serve the HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"

	"digiwal/services/wallet-service/internal/api"
	"digiwal/services/wallet-service/internal/authz"
	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
)

func main() {
	initSeed := flag.String("init-seed", "", "generate + seal + store the seed for this role (user_derivation|hot_wallet), print the mnemonic once, then exit")
	flag.Parse()

	ctx := context.Background()

	masterKeyHex := requireEnv("MASTER_KEY_HEX")
	sealer, err := crypto.NewSealer(masterKeyHex)
	if err != nil {
		log.Fatal(err)
	}

	store, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if *initSeed != "" {
		if *initSeed != storage.RoleUserDerivation && *initSeed != storage.RoleHotWallet {
			log.Fatalf("unknown seed role %q (want %s or %s)", *initSeed, storage.RoleUserDerivation, storage.RoleHotWallet)
		}
		runInit(ctx, store, sealer, *initSeed)
		return
	}

	runServer(ctx, store, sealer)
}

func runInit(ctx context.Context, store *storage.Store, sealer *crypto.Sealer, role string) {
	mnemonic, err := hdwallet.NewMnemonic()
	if err != nil {
		log.Fatalf("generating mnemonic: %v", err)
	}
	sealed, err := sealer.Seal([]byte(mnemonic))
	if err != nil {
		log.Fatalf("sealing mnemonic: %v", err)
	}
	if err := store.InitSeed(ctx, role, sealed); err != nil {
		log.Fatalf("storing %s seed: %v (if you meant to rotate it, that's not supported by this flag)", role, err)
	}

	fmt.Println("=====================================================================")
	fmt.Printf(" %s SEED CREATED. THIS MNEMONIC WILL NEVER BE SHOWN AGAIN.\n", strings.ToUpper(role))
	fmt.Println(" Write it down and store it offline. Anyone with this phrase can")
	fmt.Println(" derive and spend from every address this system ever generates.")
	fmt.Println("=====================================================================")
	fmt.Println()
	fmt.Println(mnemonic)
	fmt.Println()
	fmt.Println("=====================================================================")
}

func runServer(ctx context.Context, store *storage.Store, sealer *crypto.Sealer) {
	rdb := redis.NewClient(&redis.Options{Addr: requireEnv("REDIS_ADDR")})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connecting to redis: %v", err)
	}
	cache := storage.NewCache(rdb)

	var signer api.Signer
	userWallet, err := loadWallet(ctx, store, sealer, storage.RoleUserDerivation)
	switch {
	case err == nil:
		signer = userWallet
		log.Println("user_derivation wallet loaded")
	case errors.Is(err, storage.ErrNotFound):
		log.Println("WARNING: no user_derivation seed found — deposit-address requests will fail until you run with -init-seed user_derivation")
	default:
		log.Fatalf("loading user_derivation wallet: %v", err)
	}

	// The Hot Wallet is a separate seed (wallet-tiering.md invariant #1),
	// one address at BIP44 index 0. Nothing signs with it yet (task 0.3);
	// loading it here surfaces a wrong MASTER_KEY_HEX or corrupt seed at boot.
	hotWallet, err := loadWallet(ctx, store, sealer, storage.RoleHotWallet)
	switch {
	case err == nil:
		addr, err := hotWallet.AddressAt(0)
		if err != nil {
			log.Fatalf("deriving hot wallet address: %v", err)
		}
		log.Printf("hot_wallet wallet loaded, address %s", addr)
	case errors.Is(err, storage.ErrNotFound):
		log.Println("WARNING: no hot_wallet seed found — run with -init-seed hot_wallet before Hot Wallet signing is used")
	default:
		log.Fatalf("loading hot_wallet wallet: %v", err)
	}

	server := api.NewServer(store, cache, signer)

	// mTLS listener for /v1/sign/* (service-auth-mtls.md). Fail closed: without
	// certs it simply isn't started, so nothing can be signed.
	if certFile := os.Getenv("TLS_CERT_FILE"); certFile != "" {
		tlsCfg, err := authz.ServerTLSConfig(certFile, requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("configuring mTLS: %v", err)
		}
		mtlsPort := envOr("MTLS_PORT", "8443")
		srv := &http.Server{Addr: ":" + mtlsPort, Handler: server.MTLSRoutes(), TLSConfig: tlsCfg}
		go func() {
			log.Printf("wallet-service mTLS listening on :%s", mtlsPort)
			log.Fatal(srv.ListenAndServeTLS("", ""))
		}()
	} else {
		log.Println("WARNING: TLS_CERT_FILE not set — mTLS listener disabled, signing endpoints unavailable (run `make certs`)")
	}

	port := envOr("PORT", "8080")
	log.Printf("wallet-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, server.Routes()))
}

// loadWallet decrypts the sealed seed for a role and builds an in-memory
// wallet from it. Returns storage.ErrNotFound if the role's seed hasn't
// been initialized yet.
func loadWallet(ctx context.Context, store *storage.Store, sealer *crypto.Sealer, role string) (*hdwallet.Wallet, error) {
	sealed, err := store.SealedSeed(ctx, role)
	if err != nil {
		return nil, err
	}
	mnemonic, err := sealer.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypting %s seed (wrong MASTER_KEY_HEX?): %w", role, err)
	}
	return hdwallet.FromMnemonic(string(mnemonic))
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
