//go:build !prod

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

// The software Hot Wallet (dev profile, key-custody-hsm.md invariant 7). This whole file is absent from a `-tags prod` build,
// so the production binary has no seed code, no hdwallet import and no MASTER_KEY_HEX handling. The user_derivation seed is not
// here and never was in this file: it is sweep-signer's.

// loadSoftwareHot opens the sealed hot_wallet seed with MASTER_KEY_HEX and wraps it at the Hot index. storage.ErrNotFound while the
// seed has not been initialised (`service -init-seed hot_wallet`).
func loadSoftwareHot(ctx context.Context, store *storage.Store) (signing.HotBackend, error) {
	sealer, err := crypto.NewSealer(requireEnv("MASTER_KEY_HEX"))
	if err != nil {
		return nil, err
	}
	sealed, err := store.SealedSeed(ctx, storage.RoleHotWallet)
	if err != nil {
		return nil, err
	}
	mnemonic, err := sealer.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypting the hot_wallet seed (wrong MASTER_KEY_HEX?): %w", err)
	}
	w, err := hdwallet.FromMnemonic(string(mnemonic))
	if err != nil {
		return nil, err
	}
	log.Println("Hot Key: software, from the sealed hot_wallet seed (SIGNER_BACKEND=software, dev profile)")
	return signing.SoftwareHot(w, hdwallet.HotWalletIndex), nil
}

// runInitSeed is `service -init-seed hot_wallet`.
func runInitSeed(ctx context.Context, store *storage.Store, role string) {
	switch role {
	case storage.RoleUserDerivation:
		log.Fatal("the user_derivation seed is no longer created by wallet-service: run `key-admin init-seed user_derivation` (key-custody-hsm.md M3b)")
	case storage.RoleHotWallet:
	default:
		log.Fatalf("unknown seed role %q (want %s)", role, storage.RoleHotWallet)
	}
	if signerBackend() == signing.BackendPKCS11 {
		log.Fatal("SIGNER_BACKEND=pkcs11: the Hot Key lives in the HSM, there is no hot_wallet seed to create")
	}
	sealer, err := crypto.NewSealer(requireEnv("MASTER_KEY_HEX"))
	if err != nil {
		log.Fatal(err)
	}
	mnemonic, err := hdwallet.NewMnemonic()
	if err != nil {
		log.Fatalf("generating mnemonic: %v", err)
	}
	sealed, err := sealer.Seal([]byte(mnemonic))
	if err != nil {
		log.Fatalf("sealing mnemonic: %v", err)
	}
	if err := store.InitSeed(ctx, role, sealed); err != nil {
		if errors.Is(err, storage.ErrAlreadyInitialized) {
			log.Fatalf("storing %s seed: %v (if you meant to rotate it, that's not supported by this flag)", role, err)
		}
		log.Fatalf("storing %s seed: %v", role, err)
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
