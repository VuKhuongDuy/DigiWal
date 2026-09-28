// Command service is wallet-service. Two modes:
//
//	service -init-master-wallet   generate the one master seed, seal it, store it, print the
//	                              mnemonic once, exit. Run this exactly once, ever.
//	service                       normal mode: decrypt the sealed seed already in Postgres
//	                              (using the same MASTER_KEY_HEX) and serve the HTTP API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/redis/go-redis/v9"

	"digiwal/services/wallet-service/internal/api"
	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
)

func main() {
	initMasterWallet := flag.Bool("init-master-wallet", false, "generate + seal + store the master wallet seed, print the mnemonic once, then exit")
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

	if *initMasterWallet {
		runInit(ctx, store, sealer)
		return
	}

	runServer(ctx, store, sealer)
}

func runInit(ctx context.Context, store *storage.Store, sealer *crypto.Sealer) {
	mnemonic, err := hdwallet.NewMnemonic()
	if err != nil {
		log.Fatalf("generating mnemonic: %v", err)
	}
	sealed, err := sealer.Seal([]byte(mnemonic))
	if err != nil {
		log.Fatalf("sealing mnemonic: %v", err)
	}
	if err := store.InitMasterWallet(ctx, sealed); err != nil {
		log.Fatalf("storing master wallet: %v (if you meant to rotate it, that's not supported by this flag)", err)
	}

	fmt.Println("=====================================================================")
	fmt.Println(" MASTER WALLET CREATED. THIS MNEMONIC WILL NEVER BE SHOWN AGAIN.")
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
	sealedSeed, err := store.SealedSeed(ctx)
	switch {
	case err == nil:
		mnemonic, err := sealer.Open(sealedSeed)
		if err != nil {
			log.Fatalf("decrypting master wallet seed (wrong MASTER_KEY_HEX?): %v", err)
		}
		signer, err = api.LoadOrInitSigner(ctx, string(mnemonic))
		if err != nil {
			log.Fatalf("loading master wallet: %v", err)
		}
		log.Println("master wallet loaded")
	case err == storage.ErrNotFound:
		log.Println("WARNING: no master wallet found — deposit-address requests will fail until you run with -init-master-wallet")
	default:
		log.Fatalf("reading master wallet: %v", err)
	}

	server := api.NewServer(store, cache, signer)

	port := envOr("PORT", "8080")
	log.Printf("wallet-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, server.Routes()))
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
