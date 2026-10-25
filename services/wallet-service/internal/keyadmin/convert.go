//go:build !prod

package keyadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"digiwal/services/wallet-service/internal/crypto"
	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/xpub"
)

// ConvertStore is the slice of storage.Store the conversion uses.
type ConvertStore interface {
	AllAddresses(ctx context.Context, chain string) ([]storage.AddressRow, error)
	SealedSeed(ctx context.Context, role string) ([]byte, error)
	SealedSeed64(ctx context.Context, role string) ([]byte, error)
	SetSealedSeed64(ctx context.Context, role string, blob []byte) error
	XPub(ctx context.Context, chain string) (storage.XPubRow, error)
}

// floor is how many leading indexes are always cross-checked, so an environment with few (or no) issued addresses still gets
// a real proof that the new derivation equals the old one.
const floor = 100

// ConvertSeed is `key-admin convert-seed [-dry-run]` (key-custody-hsm.md K11, M3b): the one-off conversion of the sealed
// user_derivation MNEMONIC into the sealed 64-byte Seed that sweep-signer reads. In order, stopping at the first failure:
//
//  1. open the mnemonic-format row with the master key, derive the 64-byte BIP39 seed (the only place a mnemonic string
//     becomes the Seed; this is a short-lived ceremony process, not a service);
//  2. PROVE the new derivation (hdkeychain + btcec on the Seed bytes, what sweep-signer runs) reproduces EVERY existing
//     wallet_addresses row AND the first 100 indexes of the old derivation (go-ethereum-hdwallet from the mnemonic): any
//     difference REFUSES and writes nothing;
//  3. if wallet_xpubs exists, the Seed's account xpub must be the stored one (else the Seed is not the one the attested xpub
//     belongs to: REFUSED);
//  4. seal the Seed (same master key, AAD crypto.SeedAAD), open the result again and compare, then (not in a dry run) write
//     it to wallet_seeds.sealed_seed64. The mnemonic-format blob is not touched: it stays as the rollback until M3c.
//
// Re-running on a converted environment only re-verifies it. Nothing secret is printed: counts and the xpub fingerprint.
func ConvertSeed(ctx context.Context, st ConvertStore, sealer *crypto.Sealer, dryRun bool, out io.Writer) error {
	role := storage.RoleUserDerivation
	rows, err := st.AllAddresses(ctx, xpub.ChainEVM)
	if err != nil {
		return err
	}

	switch existing, err := st.SealedSeed64(ctx, role); {
	case err == nil:
		seed, err := sealer.OpenAAD(existing, []byte(crypto.SeedAAD))
		if err != nil {
			return fmt.Errorf("REFUSED: the stored sealed_seed64 does not open with this MASTER_KEY_HEX and AAD %q: %w", crypto.SeedAAD, err)
		}
		defer clear(seed)
		if err := proveSeed(ctx, st, seed, nil, rows, out); err != nil {
			return err
		}
		fmt.Fprintln(out, "already converted: the stored 64-byte Seed opens and reproduces every issued address (nothing written)")
		return nil
	case errors.Is(err, storage.ErrNotFound):
	default:
		return err
	}

	blob, err := st.SealedSeed(ctx, role)
	if errors.Is(err, storage.ErrNotFound) {
		return errors.New("nothing to convert: no mnemonic-format user_derivation seed (a new environment uses `key-admin init-seed user_derivation`)")
	}
	if err != nil {
		return err
	}
	mnemonic, err := sealer.Open(blob)
	if err != nil {
		return fmt.Errorf("decrypting the user_derivation seed (wrong MASTER_KEY_HEX?): %w", err)
	}
	legacy, err := hdwallet.FromMnemonic(string(mnemonic))
	if err != nil {
		return err
	}
	seed, err := hdwallet.SeedFromMnemonic(string(mnemonic))
	clear(mnemonic)
	if err != nil {
		return err
	}
	defer clear(seed)
	if err := proveSeed(ctx, st, seed, legacy, rows, out); err != nil {
		return err
	}

	sealed, err := sealer.SealAAD(seed, []byte(crypto.SeedAAD))
	if err != nil {
		return err
	}
	back, err := sealer.OpenAAD(sealed, []byte(crypto.SeedAAD))
	if err != nil || !bytes.Equal(back, seed) {
		clear(back)
		return errors.New("REFUSED: the freshly sealed Seed does not open back to the same bytes, nothing stored")
	}
	clear(back)
	if _, err := sealer.Open(sealed); err == nil {
		return errors.New("REFUSED: the sealed Seed also opens with no AAD: the format is not bound, nothing stored")
	}
	if dryRun {
		fmt.Fprintln(out, "DRY RUN: the Seed would be sealed (AES-256-GCM, AAD "+crypto.SeedAAD+") and written to wallet_seeds.sealed_seed64. Nothing was written.")
		return nil
	}
	if err := st.SetSealedSeed64(ctx, role, sealed); err != nil {
		return err
	}
	fmt.Fprintln(out, "converted: wallet_seeds.sealed_seed64 written. The mnemonic-format blob is untouched (rollback until M3c). Start sweep-signer.")
	return nil
}

// proveSeed is step 2 and 3. legacy may be nil (the re-verify path has no mnemonic).
func proveSeed(ctx context.Context, st ConvertStore, seed []byte, legacy *hdwallet.Wallet, rows []storage.AddressRow, out io.Writer) error {
	xp, err := hdwallet.AccountXPubFromSeed(seed)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Seed account xpub m/44'/60'/0' fingerprint %s\n", xpub.Fingerprint(xp))

	for _, r := range rows {
		got, err := hdwallet.AddressAtSeed(seed, r.Index)
		if err != nil {
			return err
		}
		if got != r.Address {
			return fmt.Errorf("REFUSED: wallet_addresses row for user %q index %d holds %s but the 64-byte Seed derives %s: this Seed does not reproduce the issued addresses, nothing was written", r.UserID, r.Index, r.Address, got)
		}
	}
	fmt.Fprintf(out, "reproduced %d/%d issued wallet_addresses rows from the 64-byte Seed\n", len(rows), len(rows))

	if legacy != nil {
		for i := uint32(0); i < floor; i++ {
			a, err := hdwallet.AddressAtSeed(seed, i)
			if err != nil {
				return err
			}
			b, err := legacy.AddressAt(i)
			if err != nil {
				return err
			}
			if a != b {
				return fmt.Errorf("REFUSED: index %d: the new derivation gives %s, the old one %s, nothing was written", i, a, b)
			}
		}
		fmt.Fprintf(out, "indexes 0-%d: the new derivation equals the old one (go-ethereum-hdwallet)\n", floor-1)
	}

	switch row, err := st.XPub(ctx, xpub.ChainEVM); {
	case err == nil:
		if row.XPub != xp {
			return errors.New("REFUSED: the Seed's account xpub is not the xpub stored in wallet_xpubs: this is not the Seed the attested xpub belongs to, nothing was written")
		}
		fmt.Fprintln(out, "the Seed's account xpub equals the attested xpub in wallet_xpubs")
	case errors.Is(err, storage.ErrNotFound):
		fmt.Fprintln(out, "wallet_xpubs is empty (run `key-admin migrate-xpub` too)")
	default:
		return err
	}
	return nil
}
