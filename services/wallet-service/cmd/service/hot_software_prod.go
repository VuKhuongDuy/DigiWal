//go:build prod

package main

import (
	"context"
	"errors"
	"log"

	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

// A production build has no software signer (key-custody-hsm.md invariant 7, K14): the Hot Key is in the HSM or there is none.
func loadSoftwareHot(context.Context, *storage.Store) (signing.HotBackend, error) {
	return nil, errors.New("SIGNER_BACKEND=software is not compiled into a prod build: set SIGNER_BACKEND=pkcs11")
}

func runInitSeed(_ context.Context, _ *storage.Store, _ string) {
	log.Fatal("-init-seed is not compiled into a prod build (the Hot Key is created in the HSM; the user_derivation seed by `key-admin init-seed`)")
}
