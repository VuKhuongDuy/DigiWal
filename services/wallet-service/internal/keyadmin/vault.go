package keyadmin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"

	"digiwal/services/wallet-service/internal/seedkit"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/vaultstore"
	"digiwal/services/wallet-service/internal/xpub"
)

// The vault ceremonies (key-custody-hsm.md invariants 18-21, 5.4, M3c): fresh initialisation of the user_derivation Seed, and the
// rotations of its wrappers. The seed exists in this process only during a fresh init (it has to be shown once as a mnemonic); every
// rotation is performed INSIDE hsm-gateway/the token, and this process only ever sees the public account xpub.

// Role and purpose of the one Sealed Secret.
const (
	RoleUserDerivation = "user_derivation"
	PurposeSweep       = "sweep"
	// ParitySample is how many leading indexes a fresh init proves: private derivation from the seed equals public derivation from the xpub.
	ParitySample = 50
)

// VaultStore is the slice of vaultstore.Store the ceremonies use.
type VaultStore interface {
	List(ctx context.Context, role string) ([]vaultstore.Row, error)
	Insert(ctx context.Context, r vaultstore.Row) error
	Activate(ctx context.Context, role string, version int) error
	DeletePending(ctx context.Context, role string, version int) error
}

// XPubStore is the slice of the custody database the ceremonies read: the attested xpub row.
type XPubStore interface {
	XPub(ctx context.Context, chain string) (storage.XPubRow, error)
}

// Deps are the collaborators of the ceremonies.
type Deps struct {
	Vault   VaultStore
	GW      VaultGateway
	Attest  Gateway   // the attestation half of hsm-gateway (fresh init only)
	Custody Store     // wallet_addresses / wallet_xpubs (fresh init only)
	XStore  XPubStore // wallet_xpubs (rotations)
	Pin     *ecdsa.PublicKey
	Out     io.Writer
	// NewSeed generates the mnemonic and its 64-byte seed (fresh init); nil means seedkit.NewMnemonic + SeedFromMnemonic.
	NewSeed func() (mnemonic string, seed []byte, err error)
}

func (d Deps) newSeed() (string, []byte, error) {
	if d.NewSeed != nil {
		return d.NewSeed()
	}
	m, err := seedkit.NewMnemonic()
	if err != nil {
		return "", nil, err
	}
	s, err := seedkit.SeedFromMnemonic(m)
	return m, s, err
}

// freshStore is the custody Store of a fresh init: the xpub counter starts at 0 (there is no old seed counter to carry over).
type freshStore struct{ Store }

func (freshStore) UserDerivationNextIndex(context.Context) (uint32, error) { return 0, nil }

// ErrInitialised: a Sealed Secret already exists for the role. Fresh init refuses; the rotation commands are the way to change it.
var ErrInitialised = errors.New("REFUSED: a sealed secret already exists for user_derivation: key-admin init never replaces it (rotate-kek, rotate-dek and rewrap change its wrappers; a new Seed is the Seed rotation runbook)")

func active(rows []vaultstore.Row) (vaultstore.Row, bool) {
	for _, r := range rows {
		if r.Status == vaultstore.Active {
			return r, true
		}
	}
	return vaultstore.Row{}, false
}

func maxVersion(rows []vaultstore.Row) int {
	m := 0
	for _, r := range rows {
		if r.Version > m {
			m = r.Version
		}
	}
	return m
}

// FreshInit is `key-admin init`: the one-off initialisation of a new environment. In order, stopping at the first failure:
//
//  1. refuse if any Sealed Secret of the role exists (unless resume, which only completes an interrupted init of a PENDING row);
//  2. generate the Seed and derive its account xpub; prove, for the first ParitySample indexes, that private derivation from the Seed
//     equals public derivation from the xpub (what wallet-service issues and what sweep-signer signs);
//  3. have hsm-gateway seal it (DEK generated and wrapped inside the token, AES-256-GCM inside the token), store it PENDING;
//  4. have the gateway open the stored row and return the xpub: it must equal the local one (the sealed row really holds this Seed);
//  5. show the mnemonic ONCE (the disaster-recovery record, invariant 21: the row it backs is now stored and verified);
//  6. have the gateway attest the xpub, check the attestation against the pin, store wallet_xpubs (the migrate-xpub steps);
//  7. read the stored xpub back and repeat the parity proof against it; only then flip the row to ACTIVE, which is the moment
//     sweep-signer can read it.
//
// An interruption leaves a pending row that sweep-signer cannot read: `init -resume` finishes it from the gateway alone.
func FreshInit(ctx context.Context, d Deps, resume bool) error {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		if _, ok := active(rows); ok || !resume || len(rows) != 1 || rows[0].Status != vaultstore.Pending {
			return ErrInitialised
		}
		return resumeInit(ctx, d, rows[0])
	}
	if resume {
		return errors.New("nothing to resume: no pending sealed secret (run key-admin init)")
	}

	mnemonic, seed, err := d.newSeed()
	if err != nil {
		return fmt.Errorf("generating the seed: %w", err)
	}
	defer clear(seed)
	xp, err := seedkit.AccountXPubFromSeed(seed)
	if err != nil {
		return err
	}
	if err := parity(seed, xp, d.Out); err != nil {
		return err
	}

	sealed, err := d.GW.Seal(ctx, RoleUserDerivation, PurposeSweep, 1, 0, seed)
	if err != nil {
		return fmt.Errorf("sealing the seed through hsm-gateway: %w", err)
	}
	row := RowOf(sealed)
	if err := d.Vault.Insert(ctx, row); err != nil {
		return fmt.Errorf("storing the sealed secret: %w", err)
	}
	fmt.Fprintf(d.Out, "sealed: version 1 stored as pending (kek-v%d, wrapped DEK %d bytes, AES-256-GCM ciphertext %d bytes)\n", row.KEKVersion, len(row.WrappedDEK), len(row.Ciphertext))

	got, err := d.GW.VerifyXPub(ctx, sealed)
	if err != nil {
		return fmt.Errorf("opening the stored row through hsm-gateway: %w (the row stays pending; nothing can read it)", err)
	}
	if got != xp {
		return errors.New("REFUSED: the sealed row opens to a different seed than the one just generated; the pending row is left for inspection and nothing was attested")
	}
	fmt.Fprintf(d.Out, "verified: hsm-gateway opens the stored row inside the HSM and derives the same account xpub (fingerprint %s)\n", xpub.Fingerprint(xp))

	banner(d.Out, mnemonic)

	if err := attestAndStore(ctx, d, xp); err != nil {
		return fmt.Errorf("%w (the mnemonic above is the backup of the pending row; run `key-admin init -resume` to finish)", err)
	}
	stored, err := d.Custody.XPub(ctx, xpub.ChainEVM)
	if err != nil {
		return err
	}
	if stored.XPub != xp {
		return errors.New("REFUSED: the stored xpub is not the one just derived")
	}
	if err := parity(seed, stored.XPub, d.Out); err != nil {
		return err
	}
	if err := d.Vault.Activate(ctx, RoleUserDerivation, 1); err != nil {
		return fmt.Errorf("activating version 1: %w", err)
	}
	fmt.Fprintf(d.Out, "INITIALISED: user_derivation version 1 is active under kek-v%d; xpub attested and stored; address parity proven for %d indexes\n", row.KEKVersion, ParitySample)
	return nil
}

func resumeInit(ctx context.Context, d Deps, row vaultstore.Row) error {
	xp, err := d.GW.VerifyXPub(ctx, SealedOf(row))
	if err != nil {
		return fmt.Errorf("opening the pending row through hsm-gateway: %w", err)
	}
	switch stored, err := d.Custody.XPub(ctx, xpub.ChainEVM); {
	case err == nil:
		if stored.XPub != xp {
			return errors.New("REFUSED: wallet_xpubs holds a different xpub than the pending row's seed derives")
		}
		fmt.Fprintln(d.Out, "resume: the stored xpub is the pending row's")
	case errors.Is(err, storage.ErrNotFound):
		if err := attestAndStore(ctx, d, xp); err != nil {
			return err
		}
	default:
		return err
	}
	if err := d.Vault.Activate(ctx, RoleUserDerivation, row.Version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "INITIALISED (resumed): user_derivation version %d is active (xpub fingerprint %s)\n", row.Version, xpub.Fingerprint(xp))
	return nil
}

func attestAndStore(ctx context.Context, d Deps, xp string) error {
	return MigrateXPub(ctx, freshStore{d.Custody}, d.Attest, d.Pin, xp, false, d.Out)
}

// parity proves private derivation from the Seed equals public derivation from the xpub for the first ParitySample indexes.
func parity(seed []byte, xp string, out io.Writer) error {
	acct, err := xpub.ParseAccountKey(xp)
	if err != nil {
		return err
	}
	for i := uint32(0); i < ParitySample; i++ {
		priv, err := seedkit.AddressAtSeed(seed, i)
		if err != nil {
			return err
		}
		pub, err := xpub.AddressAt(acct, i)
		if err != nil {
			return err
		}
		if priv != pub {
			return fmt.Errorf("REFUSED: index %d: the seed derives %s, the xpub derives %s: deposit addresses would not be spendable, nothing stored", i, priv, pub)
		}
	}
	fmt.Fprintf(out, "parity: private derivation from the seed equals public derivation from the xpub for indexes 0-%d\n", ParitySample-1)
	return nil
}

func banner(out io.Writer, mnemonic string) {
	fmt.Fprintln(out, "=====================================================================")
	fmt.Fprintln(out, " USER_DERIVATION SEED CREATED. THIS MNEMONIC WILL NEVER BE SHOWN AGAIN.")
	fmt.Fprintln(out, " Write it down and store it offline: it is the disaster-recovery record")
	fmt.Fprintln(out, " of the sealed seed. Anyone with this phrase can derive and spend from")
	fmt.Fprintln(out, " every deposit address this system ever generates.")
	fmt.Fprintln(out, "=====================================================================")
	fmt.Fprintln(out)
	fmt.Fprintln(out, mnemonic)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "=====================================================================")
}

// ---- rotations

// RotateOpts selects a rotation.
type RotateOpts struct {
	// ToKEK is the KEK version that wraps the new row; 0 = rotate-kek's choice (the newest wrapping KEK) or, for a DEK rotation, the active row's KEK.
	ToKEK int
	// NewDEK also replaces the DEK (rotate-dek). false keeps the DEK and only re-wraps it (rewrap, rotate-kek).
	NewDEK bool
	// Flip activates the verified pending row (rotate-kek and rotate-dek); false leaves it pending (rewrap).
	Flip bool
	// GenerateKEK makes rotate-kek create the next KEK version first when the active row already uses the newest one.
	GenerateKEK bool
}

// Rotate is the shared engine of rewrap, rotate-kek and rotate-dek. It is idempotent and interruption-safe:
//
//   - nothing to do (the active row already is under the target KEK and no new DEK was asked for) returns nil, changing nothing;
//   - an inert PENDING row from an interrupted run is reused when it is verified and fits the request, else deleted and redone;
//   - the new row is written PENDING, opened inside the gateway and its xpub compared with the attested xpub in wallet_xpubs (and the
//     attestation with the pin) BEFORE anything can read it; a mismatch deletes it and refuses;
//   - the flip is one transaction (pending -> active, active -> retired). The old row stays for rollback while its KEK exists.
func Rotate(ctx context.Context, d Deps, o RotateOpts) error {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return err
	}
	cur, ok := active(rows)
	if !ok {
		return errors.New("no active user_derivation secret: run key-admin init first")
	}
	keks, err := d.GW.KEKs(ctx)
	if err != nil {
		return fmt.Errorf("listing the KEKs: %w", err)
	}
	newest := 0
	for _, k := range keks {
		if k.Wrap && k.Version > newest {
			newest = k.Version
		}
	}
	target := o.ToKEK
	switch {
	case target != 0:
	case o.NewDEK && !o.GenerateKEK:
		target = cur.KEKVersion
	default:
		target = newest
		if o.GenerateKEK && cur.KEKVersion >= newest {
			hi := 0
			for _, k := range keks {
				if k.Version > hi {
					hi = k.Version
				}
			}
			created, err := d.GW.GenerateKEK(ctx, hi+1)
			if err != nil {
				return fmt.Errorf("generating kek-v%d: %w", hi+1, err)
			}
			fmt.Fprintf(d.Out, "kek-v%d generated inside the token (created=%v)\n", hi+1, created)
			target = hi + 1
		}
	}
	if !o.NewDEK && cur.KEKVersion == target {
		fmt.Fprintf(d.Out, "nothing to do: the active row (version %d) is already wrapped under kek-v%d\n", cur.Version, target)
		return nil
	}

	x, err := d.XStore.XPub(ctx, xpub.ChainEVM)
	if err != nil {
		return fmt.Errorf("reading the attested xpub: %w", err)
	}
	if d.Pin != nil {
		if _, err := xpub.Verify(d.Pin, x.Record()); err != nil {
			return fmt.Errorf("the stored xpub row does not verify against the pin, nothing rotated: %w", err)
		}
	}

	// an interrupted earlier run left a pending row: reuse it only if it is verified and is what was asked for
	var pend *vaultstore.Row
	for i := range rows {
		if rows[i].Status != vaultstore.Pending {
			continue
		}
		p := rows[i]
		fits := p.KEKVersion == target && (!o.NewDEK || !bytes.Equal(p.WrappedDEK, cur.WrappedDEK))
		if got, err := d.GW.VerifyXPub(ctx, SealedOf(p)); fits && err == nil && got == x.XPub {
			pend = &p
			fmt.Fprintf(d.Out, "resuming: pending version %d from an interrupted run is verified and fits\n", p.Version)
			continue
		}
		if err := d.Vault.DeletePending(ctx, RoleUserDerivation, p.Version); err != nil {
			return err
		}
		fmt.Fprintf(d.Out, "deleted the stale pending version %d of an interrupted run (it was inert: nothing can read a pending row)\n", p.Version)
	}

	if pend == nil {
		rows, err = d.Vault.List(ctx, RoleUserDerivation)
		if err != nil {
			return err
		}
		nv := maxVersion(rows) + 1
		ns, err := d.GW.Reseal(ctx, SealedOf(cur), nv, target, o.NewDEK)
		if err != nil {
			return fmt.Errorf("re-sealing inside the token: %w", err)
		}
		nr := RowOf(ns)
		if err := d.Vault.Insert(ctx, nr); err != nil {
			return fmt.Errorf("storing pending version %d: %w", nv, err)
		}
		fmt.Fprintf(d.Out, "pending version %d written (kek-v%d, new DEK: %v); the active version %d is untouched\n", nv, nr.KEKVersion, o.NewDEK, cur.Version)
		pend = &nr
		got, err := d.GW.VerifyXPub(ctx, SealedOf(nr))
		if err != nil || got != x.XPub {
			_ = d.Vault.DeletePending(ctx, RoleUserDerivation, nv)
			if err != nil {
				return fmt.Errorf("REFUSED: the pending row does not open (deleted): %w", err)
			}
			return errors.New("REFUSED: the pending row opens to a different seed than the attested xpub's (deleted); nothing changed")
		}
	}
	fmt.Fprintf(d.Out, "verified: pending version %d opens inside the HSM to the attested xpub (fingerprint %s)\n", pend.Version, xpub.Fingerprint(x.XPub))
	if !o.Flip {
		fmt.Fprintf(d.Out, "left PENDING (rewrap): `key-admin activate` flips it, the old version stays active until then\n")
		return nil
	}
	if err := d.Vault.Activate(ctx, RoleUserDerivation, pend.Version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "flipped: version %d is active, version %d is retired and kept for rollback while kek-v%d exists\n", pend.Version, cur.Version, cur.KEKVersion)
	return nil
}

// Activate flips the single verified pending row (the second half of rewrap).
func Activate(ctx context.Context, d Deps) error {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return err
	}
	var pend []vaultstore.Row
	for _, r := range rows {
		if r.Status == vaultstore.Pending {
			pend = append(pend, r)
		}
	}
	if len(pend) != 1 {
		return fmt.Errorf("want exactly one pending row to activate, found %d", len(pend))
	}
	if err := verifyAgainstAttested(ctx, d, pend[0]); err != nil {
		return err
	}
	if err := d.Vault.Activate(ctx, RoleUserDerivation, pend[0].Version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "flipped: version %d is active\n", pend[0].Version)
	return nil
}

// Rollback flips back to the newest retired row, if its KEK is still in the token and it still opens to the attested xpub.
func Rollback(ctx context.Context, d Deps) error {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return err
	}
	cur, ok := active(rows)
	if !ok {
		return errors.New("no active row")
	}
	var prev *vaultstore.Row
	for i := range rows {
		if rows[i].Status == vaultstore.Retired && rows[i].Version < cur.Version && (prev == nil || rows[i].Version > prev.Version) {
			prev = &rows[i]
		}
	}
	if prev == nil {
		return errors.New("no retired version below the active one to roll back to")
	}
	if err := verifyAgainstAttested(ctx, d, *prev); err != nil {
		return fmt.Errorf("cannot roll back to version %d (its KEK may be destroyed): %w", prev.Version, err)
	}
	if err := d.Vault.Activate(ctx, RoleUserDerivation, prev.Version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "rolled back: version %d is active again, version %d is retired\n", prev.Version, cur.Version)
	return nil
}

func verifyAgainstAttested(ctx context.Context, d Deps, r vaultstore.Row) error {
	x, err := d.XStore.XPub(ctx, xpub.ChainEVM)
	if err != nil {
		return fmt.Errorf("reading the attested xpub: %w", err)
	}
	got, err := d.GW.VerifyXPub(ctx, SealedOf(r))
	if err != nil {
		return fmt.Errorf("version %d does not open: %w", r.Version, err)
	}
	if got != x.XPub {
		return fmt.Errorf("REFUSED: version %d opens to a different seed than the attested xpub's", r.Version)
	}
	return nil
}

// RetireKEK makes a KEK unwrap-only. Refused while the active or a pending row is wrapped under it.
func RetireKEK(ctx context.Context, d Deps, version int) error {
	if err := kekFree(ctx, d, version, false); err != nil {
		return err
	}
	if err := d.GW.RetireKEK(ctx, version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "kek-v%d is unwrap-only now (it still opens the rows it wrapped; it can no longer wrap)\n", version)
	return nil
}

// DestroyKEK deletes a KEK from the token. Refused while any live row needs it; a retired row under it makes rollback impossible, so that
// needs force.
func DestroyKEK(ctx context.Context, d Deps, version int, force bool) error {
	if err := kekFree(ctx, d, version, !force); err != nil {
		return err
	}
	if err := d.GW.DestroyKEK(ctx, version); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "kek-v%d destroyed inside the token\n", version)
	return nil
}

func kekFree(ctx context.Context, d Deps, version int, retiredToo bool) error {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.KEKVersion != version {
			continue
		}
		if r.Status == vaultstore.Active || r.Status == vaultstore.Pending || (retiredToo && r.Status == vaultstore.Retired) {
			return fmt.Errorf("REFUSED: version %d (%s) is wrapped under kek-v%d (a retired row means rollback would be lost: -force)", r.Version, r.Status, version)
		}
	}
	return nil
}

// Status prints the rows and KEKs and reports whether the role is initialised (an active row exists).
func Status(ctx context.Context, d Deps) (bool, error) {
	rows, err := d.Vault.List(ctx, RoleUserDerivation)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		fmt.Fprintf(d.Out, "user_derivation version %d  %-8s  kek-v%d  created %s\n", r.Version, r.Status, r.KEKVersion, r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if d.GW != nil {
		if keks, err := d.GW.KEKs(ctx); err == nil {
			for _, k := range keks {
				fmt.Fprintf(d.Out, "kek-v%d  wrap=%v\n", k.Version, k.Wrap)
			}
		}
	}
	_, ok := active(rows)
	if !ok {
		fmt.Fprintln(d.Out, "not initialised: no active user_derivation secret")
	}
	return ok, nil
}
