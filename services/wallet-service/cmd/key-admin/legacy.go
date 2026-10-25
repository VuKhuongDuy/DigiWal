//go:build !prod

// LEGACY tooling of key-admin (pre-M3c): everything here opens or creates seeds under the old software key MASTER_KEY_HEX through
// crypto.Sealer. It exists only for the dev/e2e regression baseline and the rehearsal of the old migrations, is compiled out of a
// `-tags prod` build, and is not wired into any compose bring-up of a real node. The Tech Lead has not decided whether to delete it.
// Nothing here is part of the M3c flow (`key-admin init`, rotations): sweep-signer no longer reads what these commands write.
//
//	key-admin migrate-xpub [-dry-run] [-dev-attester]   LEGACY: derive the xpub from an old master-key-sealed seed
//	key-admin convert-seed [-dry-run]                    LEGACY: seal the old mnemonic as a master-key 64-byte seed
//	key-admin init-seed [-legacy-mnemonic-format] user_derivation   LEGACY: create a master-key-sealed seed
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/keyadmin"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/xpub"
)

func init() {
	legacy["migrate-xpub"] = migrateXPub
	legacy["convert-seed"] = convertSeed
	legacy["init-seed"] = initSeed
}

func connect(ctx context.Context) (*storage.Store, *crypto.Sealer) {
	sealer, err := crypto.NewSealer(requireEnv("MASTER_KEY_HEX"))
	if err != nil {
		log.Fatal(err)
	}
	store, err := storage.Connect(ctx, requireEnv("POSTGRES_DSN"))
	if err != nil {
		log.Fatal(err)
	}
	return store, sealer
}

func convertSeed(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("convert-seed", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "derive the Seed and prove it reproduces every existing address, but write nothing")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		log.Fatal(usage)
	}
	store, sealer := connect(ctx)
	defer store.Close()
	if err := keyadmin.ConvertSeed(ctx, store, sealer, *dry, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func initSeed(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("init-seed", flag.ExitOnError)
	legacy := fs.Bool("legacy-mnemonic-format", false, "store the old mnemonic format (rehearses an environment that predates M3b: run convert-seed next)")
	_ = fs.Parse(args)
	args = fs.Args()
	if len(args) != 1 || args[0] != storage.RoleUserDerivation {
		log.Fatal(usage + " (only the user_derivation role: the Hot Key is in the HSM, and the software hot_wallet seed is created by `service -init-seed hot_wallet` in a dev profile)")
	}
	store, sealer := connect(ctx)
	defer store.Close()
	mnemonic, err := hdwallet.NewMnemonic()
	if err != nil {
		log.Fatalf("generating mnemonic: %v", err)
	}
	seed, err := hdwallet.SeedFromMnemonic(mnemonic)
	if err != nil {
		log.Fatal(err)
	}
	defer clear(seed)
	if *legacy {
		sealed, err := sealer.Seal([]byte(mnemonic))
		if err != nil {
			log.Fatalf("sealing the mnemonic: %v", err)
		}
		if err := store.InitSeed(ctx, storage.RoleUserDerivation, sealed); err != nil {
			log.Fatalf("storing the user_derivation seed: %v (it already exists: rotating it is not supported by this command)", err)
		}
	} else {
		sealed, err := sealer.SealAAD(seed, []byte(crypto.SeedAAD))
		if err != nil {
			log.Fatalf("sealing the seed: %v", err)
		}
		if err := store.InitSeed64(ctx, storage.RoleUserDerivation, sealed); err != nil {
			log.Fatalf("storing the user_derivation seed: %v (it already exists: rotating it is not supported by this command)", err)
		}
	}
	fmt.Println("=====================================================================")
	fmt.Println(" USER_DERIVATION SEED CREATED. THIS MNEMONIC WILL NEVER BE SHOWN AGAIN.")
	fmt.Println(" Write it down and store it offline. Anyone with this phrase can")
	fmt.Println(" derive and spend from every deposit address this system ever generates.")
	fmt.Println("=====================================================================")
	fmt.Println()
	fmt.Println(mnemonic)
	fmt.Println()
	fmt.Println("=====================================================================")
}

func migrateXPub(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate-xpub", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "derive and prove the xpub reproduces the existing addresses and compare the keys, but do not ask the gateway to attest and write nothing")
	dev := fs.Bool("dev-attester", false, "DEV/e2e without an HSM: attest with an ephemeral in-process key and print its public half for pinning")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for hsm-gateway to answer")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		log.Fatal(usage)
	}
	store, sealer := connect(ctx)
	defer store.Close()

	xp, err := accountXPub(ctx, store, sealer)
	if err != nil {
		log.Fatal(err)
	}

	var gw keyadmin.Gateway
	var pin = (*xpubPin)(nil)
	if *dev {
		da, err := keyadmin.NewDevAttester()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("attest_public_key=%s\n", da.PublicHex())
		fmt.Println("DEV attester: an ephemeral in-process key, not an HSM. Pin the line above (infra/hsm/pin-attest-key.sh) for wallet-service.")
		gw, pin = da, &xpubPin{da.Public()}
	} else {
		p, err := xpub.LoadPinned(requireEnv("XPUB_ATTEST_PUBKEY_FILE"))
		if err != nil {
			log.Fatalf("XPUB_ATTEST_PUBKEY_FILE: %v", err)
		}
		tlsCfg, err := signing.GatewayTLSConfig(requireEnv("TLS_CERT_FILE"), requireEnv("TLS_KEY_FILE"), requireEnv("TLS_CA_FILE"))
		if err != nil {
			log.Fatalf("hsm-gateway client TLS: %v", err)
		}
		g := keyadmin.NewGatewayClient(requireEnv("HSM_GATEWAY_URL"), tlsCfg)
		// The gateway may still be starting (compose brings it up in parallel): wait for it, once, up front.
		deadline := time.Now().Add(*wait)
		for {
			if _, err := g.AttestKey(ctx); err == nil {
				break
			} else if time.Now().After(deadline) {
				log.Fatalf("hsm-gateway did not answer GET /v1/attest/key within %s: %v", *wait, err)
			}
			time.Sleep(time.Second)
		}
		gw, pin = g, &xpubPin{p}
	}
	if err := keyadmin.MigrateXPub(ctx, store, gw, pin.key, xp, *dry, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// accountXPub derives the public account xpub from the sealed seed: the 64-byte format if the environment has been converted,
// else the mnemonic format. Only the public half leaves this function.
func accountXPub(ctx context.Context, store *storage.Store, sealer *crypto.Sealer) (string, error) {
	switch blob, err := store.SealedSeed64(ctx, storage.RoleUserDerivation); {
	case err == nil:
		seed, err := sealer.OpenAAD(blob, []byte(crypto.SeedAAD))
		if err != nil {
			return "", fmt.Errorf("decrypting the 64-byte user_derivation seed (wrong MASTER_KEY_HEX?): %w", err)
		}
		defer clear(seed)
		return hdwallet.AccountXPubFromSeed(seed)
	case err != storage.ErrNotFound:
		return "", err
	}
	sealed, err := store.SealedSeed(ctx, storage.RoleUserDerivation)
	if err != nil {
		return "", fmt.Errorf("reading the user_derivation seed: %w (run `key-admin init-seed user_derivation` first)", err)
	}
	mnemonic, err := sealer.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("decrypting the user_derivation seed (wrong MASTER_KEY_HEX?): %w", err)
	}
	defer clear(mnemonic)
	return hdwallet.AccountXPub(string(mnemonic))
}

type xpubPin struct{ key *ecdsa.PublicKey }
