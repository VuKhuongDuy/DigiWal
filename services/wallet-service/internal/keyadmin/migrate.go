package keyadmin

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"

	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/xpub"
)

// Store is the slice of storage.Store the migration uses.
type Store interface {
	AllAddresses(ctx context.Context, chain string) ([]storage.AddressRow, error)
	UserDerivationNextIndex(ctx context.Context) (uint32, error)
	XPub(ctx context.Context, chain string) (storage.XPubRow, error)
	InsertXPub(ctx context.Context, r storage.XPubRow) error
}

// MigrateXPub is `key-admin migrate-xpub` once the xpub has been derived from the seed (the command does that, it is the only
// part that touches the seed). In order, and stopping at the first failure:
//
//  1. every existing wallet_addresses row must be reproduced bit for bit by public derivation from this xpub (otherwise the
//     seed in RAM and the xpub disagree and nothing is written, nothing is attested);
//  2. wallet_seeds.next_index must be past every used index;
//  3. the gateway's attestation key must be the one pinned outside the database;
//  4. (dry run stops here) the gateway attests (chain, path, xpub), the attestation is verified against the pin, and the row is
//     stored with next_index carried over from wallet_seeds.
//
// Re-running with the same xpub verifies and returns nil; a different xpub is refused (rotation is M3c). The xpub itself is
// never printed: only its fingerprint.
func MigrateXPub(ctx context.Context, st Store, gw Gateway, pin *ecdsa.PublicKey, xp string, dryRun bool, out io.Writer) error {
	acct, err := xpub.ParseAccountKey(xp)
	if err != nil {
		return err
	}
	fp := xpub.Fingerprint(xp)
	fmt.Fprintf(out, "account xpub m/44'/60'/0' fingerprint %s\n", fp)

	rows, err := st.AllAddresses(ctx, xpub.ChainEVM)
	if err != nil {
		return err
	}
	var maxIdx int64 = -1
	for _, r := range rows {
		got, err := xpub.AddressAt(acct, r.Index)
		if err != nil {
			return err
		}
		if got != r.Address {
			return fmt.Errorf("REFUSED: wallet_addresses row for user %q index %d holds %s but the xpub derives %s: the xpub does not reproduce the existing addresses, nothing was attested or stored", r.UserID, r.Index, r.Address, got)
		}
		if int64(r.Index) > maxIdx {
			maxIdx = int64(r.Index)
		}
	}
	fmt.Fprintf(out, "reproduced %d/%d existing wallet_addresses rows from the xpub\n", len(rows), len(rows))

	next, err := st.UserDerivationNextIndex(ctx)
	if err != nil {
		return fmt.Errorf("reading the seed counter: %w", err)
	}
	if int64(next) <= maxIdx {
		return fmt.Errorf("REFUSED: wallet_seeds.next_index is %d but index %d is in use: the counter is behind, issuing would reuse an address", next, maxIdx)
	}

	gk, err := gw.AttestKey(ctx)
	if err != nil {
		return fmt.Errorf("asking hsm-gateway for its attestation key: %w", err)
	}
	pinned, err := xpub.ParsePinned([]byte(fmt.Sprintf("%x", gk)))
	if err != nil {
		return fmt.Errorf("hsm-gateway's attestation key: %w", err)
	}
	if xpub.PinnedHex(pinned) != xpub.PinnedHex(pin) {
		return errors.New("REFUSED: hsm-gateway's attestation public key is not the one pinned in XPUB_ATTEST_PUBKEY_FILE (wrong token, or a stale pin)")
	}

	switch cur, err := st.XPub(ctx, xpub.ChainEVM); {
	case err == nil:
		if cur.XPub != xp {
			return errors.New("REFUSED: wallet_xpubs already holds a different xpub (rotating the seed is the M3c procedure, not this command)")
		}
		if _, err := xpub.Verify(pin, cur.Record()); err != nil {
			return fmt.Errorf("the stored row for this xpub does not verify against the pin: %w", err)
		}
		fmt.Fprintf(out, "already migrated: the stored row is this xpub and its attestation verifies (next_index %d)\n", cur.NextIndex)
		return nil
	case errors.Is(err, storage.ErrNotFound):
	default:
		return err
	}

	if dryRun {
		fmt.Fprintf(out, "DRY RUN: would ask hsm-gateway to attest (evm, %s, xpub %s) and store the row with next_index %d. Nothing was attested or written.\n", xpub.PathEVM, fp, next)
		return nil
	}
	sig, pub, err := gw.Attest(ctx, xpub.ChainEVM, xpub.PathEVM, xp)
	if err != nil {
		return fmt.Errorf("attesting: %w", err)
	}
	if got, _ := xpub.ParsePinned([]byte(fmt.Sprintf("%x", pub))); got == nil || xpub.PinnedHex(got) != xpub.PinnedHex(pin) {
		return errors.New("REFUSED: the attestation was signed by a key other than the pinned one")
	}
	row := storage.XPubRow{Chain: xpub.ChainEVM, Path: xpub.PathEVM, Depth: xpub.AccountDepth, XPub: xp, Attestation: sig, NextIndex: next}
	if _, err := xpub.Verify(pin, row.Record()); err != nil {
		return fmt.Errorf("the fresh attestation does not verify against the pin, nothing stored: %w", err)
	}
	if err := st.InsertXPub(ctx, row); err != nil {
		return err
	}
	stored, err := st.XPub(ctx, xpub.ChainEVM)
	if err != nil {
		return err
	}
	if _, err := xpub.Verify(pin, stored.Record()); err != nil {
		return fmt.Errorf("the stored row does not verify: %w", err)
	}
	fmt.Fprintf(out, "migrated: wallet_xpubs stored (attested by the pinned key, next_index %d). Restart wallet-service with XPUB_ATTEST_PUBKEY_FILE set.\n", next)
	return nil
}
