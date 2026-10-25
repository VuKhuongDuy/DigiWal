package storage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/testdb"
	"digiwal/services/wallet-service/internal/xpub"
)

type hsmStandIn struct{ key *ecdsa.PrivateKey }

func newHSM(t *testing.T) *hsmStandIn {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &hsmStandIn{k}
}

func (h *hsmStandIn) sign(xp string) []byte {
	d := xpub.Digest(xpub.ChainEVM, xpub.PathEVM, xp)
	r, s, _ := ecdsa.Sign(rand.Reader, h.key, d[:])
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

// legacyWorld: a store whose user_derivation seed issued n addresses the old way (seed in RAM, wallet_seeds counter), then
// the xpub row migrated from it. Returns the wallet that "was" the seed so tests can compare.
type world struct {
	s      *Store
	w      *hdwallet.Wallet
	hsm    *hsmStandIn
	issuer *xpub.Issuer
	xp     string
}

func newWorld(t *testing.T, legacyRows int) *world {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	mn, err := hdwallet.NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	w, err := hdwallet.FromMnemonic(mn)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitSeed(ctx, RoleUserDerivation, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < legacyRows; i++ {
		if _, err := s.CreateAddress(ctx, fmt.Sprintf("legacy-%d", i), "evm", w.AddressAt); err != nil {
			t.Fatal(err)
		}
	}
	xp, err := hdwallet.AccountXPub(mn)
	if err != nil {
		t.Fatal(err)
	}
	hsm := newHSM(t)
	next, err := s.UserDerivationNextIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertXPub(ctx, XPubRow{Chain: "evm", Path: xpub.PathEVM, Depth: 3, XPub: xp, Attestation: hsm.sign(xp), NextIndex: next}); err != nil {
		t.Fatal(err)
	}
	iss, err := xpub.NewIssuer(&hsm.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &world{s: s, w: w, hsm: hsm, issuer: iss, xp: xp}
}

func (w *world) issue(ctx context.Context, user string) (string, error) {
	return w.s.CreateAddressFromXPub(ctx, user, "evm", w.issuer.AddressFor)
}

func (w *world) nextIndex(t *testing.T) uint32 {
	r, err := w.s.XPub(context.Background(), "evm")
	if err != nil {
		t.Fatal(err)
	}
	return r.NextIndex
}

// M3a: "every existing row reproduced from the xpub" and the issuance continues the same sequence.
func TestXPubIssuance_ReproducesEveryExistingRowAndContinuesTheSequence(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 120)
	rows, err := w.s.AllAddresses(ctx, "evm")
	if err != nil || len(rows) != 120 {
		t.Fatalf("legacy rows: %d %v", len(rows), err)
	}
	acct, err := xpub.ParseAccountKey(w.xp)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		got, err := xpub.AddressAt(acct, r.Index)
		if err != nil || got != r.Address {
			t.Fatalf("existing row user %s index %d: stored %s, xpub derives %s (%v)", r.UserID, r.Index, r.Address, got, err)
		}
	}
	// the stored attestation covers exactly this row, and the counter starts where the seed's stopped
	row, err := w.s.XPub(ctx, "evm")
	if err != nil || row.NextIndex != 120 {
		t.Fatalf("next_index %d (%v), want 120", row.NextIndex, err)
	}
	if _, err := xpub.Verify(&w.hsm.key.PublicKey, row.Record()); err != nil {
		t.Fatal(err)
	}
	// new users get indexes 120.. and exactly the addresses the seed would have derived
	for i := 0; i < 5; i++ {
		addr, err := w.issue(ctx, fmt.Sprintf("new-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := w.w.AddressAt(uint32(120 + i))
		idx, _ := w.s.DerivationIndexFor(ctx, fmt.Sprintf("new-%d", i), "evm")
		if addr != want || idx != uint32(120+i) {
			t.Fatalf("new user %d: %s idx %d, want %s idx %d", i, addr, idx, want, 120+i)
		}
	}
	t.Logf("120 existing wallet_addresses rows reproduced from the xpub; 5 new addresses continue at index 120 and equal the seed's")
}

// Invariant 9: issuing never reads wallet_seeds. Proved by dropping the table: issuance still works.
func TestXPubIssuance_NeverTouchesWalletSeeds(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 3)
	if _, err := w.s.pool.Exec(ctx, `DROP TABLE wallet_seeds`); err != nil {
		t.Fatal(err)
	}
	addr, err := w.issue(ctx, "after-the-seed-table-is-gone")
	if err != nil {
		t.Fatalf("issuance needs wallet_seeds: %v", err)
	}
	if want, _ := w.w.AddressAt(3); addr != want {
		t.Fatalf("%s vs %s", addr, want)
	}
}

func TestXPubIssuance_ConcurrentIssuanceNeverReusesAnIndex(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 10)
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := w.issue(ctx, fmt.Sprintf("c-%d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, _ := w.s.AllAddresses(ctx, "evm")
	var idx []int
	seen := map[string]bool{}
	for _, r := range rows {
		idx = append(idx, int(r.Index))
		if seen[r.Address] {
			t.Fatalf("address %s issued twice", r.Address)
		}
		seen[r.Address] = true
	}
	sort.Ints(idx)
	for i, v := range idx {
		if v != i {
			t.Fatalf("indexes are not 0..%d without gaps or repeats: %v", len(idx)-1, idx)
		}
	}
	if len(idx) != 10+n || w.nextIndex(t) != 10+n {
		t.Fatalf("%d rows, next_index %d, want %d", len(idx), w.nextIndex(t), 10+n)
	}
	t.Logf("%d concurrent issuances on top of 10 existing rows: indexes 0..%d, no gap, no repeat, no duplicate address", n, 10+n-1)
}

// The same user racing with itself: one wins; the losers roll back and consume NO index.
func TestXPubIssuance_SameUserRaceConsumesOneIndex(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.issue(ctx, "same-user"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 || w.nextIndex(t) != 1 {
		t.Fatalf("%d successes, next_index %d, want exactly 1 and 1", ok, w.nextIndex(t))
	}
	if a, err := w.issue(ctx, "other-user"); err != nil {
		t.Fatal(err)
	} else if want, _ := w.w.AddressAt(1); a != want {
		t.Fatalf("the next user got %s, want index 1 (%s)", a, want)
	}
}

// Invariant 10: a swapped or tampered xpub/attestation makes issuance refuse, latch and issue nothing, and consume no index.
func TestXPubIssuance_SwappedOrTamperedRowRefusesAndLatches(t *testing.T) {
	ctx := context.Background()
	attackerMn, _ := hdwallet.NewMnemonic()
	attacker, _ := hdwallet.AccountXPub(attackerMn)

	for name, tamper := range map[string]func(w *world) string{
		"xpub replaced with the attacker's (attestation kept)": func(w *world) string {
			return fmt.Sprintf(`UPDATE wallet_xpubs SET xpub = '%s'`, attacker)
		},
		"xpub replaced and re-attested by a key the pin does not know": func(w *world) string {
			other := newHSM(t)
			return fmt.Sprintf(`UPDATE wallet_xpubs SET xpub = '%s', attestation = '\x%x'`, attacker, other.sign(attacker))
		},
		"attestation bit flipped": func(w *world) string {
			a := w.hsm.sign(w.xp)
			a[10] ^= 1
			return fmt.Sprintf(`UPDATE wallet_xpubs SET attestation = '\x%x'`, a)
		},
		"attestation of another statement (path)": func(w *world) string {
			d := xpub.Digest(xpub.ChainEVM, "m/44'/60'/1'", w.xp)
			r, s, _ := ecdsa.Sign(rand.Reader, w.hsm.key, d[:])
			out := make([]byte, 64)
			r.FillBytes(out[:32])
			s.FillBytes(out[32:])
			return fmt.Sprintf(`UPDATE wallet_xpubs SET attestation = '\x%x'`, out)
		},
	} {
		w := newWorld(t, 4)
		if _, err := w.issue(ctx, "before-tamper"); err != nil {
			t.Fatal(err)
		}
		before := w.nextIndex(t)
		if _, err := w.s.pool.Exec(ctx, tamper(w)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		addr, err := w.issue(ctx, "victim")
		if err == nil || addr != "" || !errors.Is(err, xpub.ErrUnavailable) {
			t.Errorf("%s: issued %q err %v", name, addr, err)
		}
		if !w.issuer.Latched() {
			t.Errorf("%s: issuance did not latch", name)
		}
		if _, err := w.s.AddressFor(ctx, "victim", "evm"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a row was recorded for the victim (%v)", name, err)
		}
		if w.nextIndex(t) != before {
			t.Errorf("%s: the refused issuance consumed an index (%d -> %d)", name, before, w.nextIndex(t))
		}
		// existing addresses are still readable
		if _, err := w.s.AddressFor(ctx, "before-tamper", "evm"); err != nil {
			t.Errorf("%s: existing address no longer served: %v", name, err)
		}
	}
	// no row at all: refuses with ErrNotFound (not a latch: nothing was tampered with)
	w := newWorld(t, 1)
	_, _ = w.s.pool.Exec(ctx, `DELETE FROM wallet_xpubs`)
	if _, err := w.issue(ctx, "x"); !errors.Is(err, ErrNotFound) || w.issuer.Latched() {
		t.Fatalf("no row: err %v latched %v", err, w.issuer.Latched())
	}
}

func TestMigration000007_ConstraintsAndDownCarriesTheCounterBack(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	m, err := NewMigrator(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(6); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO wallet_seeds (seed_role, sealed_seed, next_index) VALUES ('user_derivation', '\x01', 7)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(7); err != nil {
		t.Fatalf("up: %v", err)
	}
	good := `INSERT INTO wallet_xpubs (chain, path, depth, xpub, attestation, next_index) VALUES ($1, $2, $3, $4, $5, $6)`
	att := make([]byte, 64)
	for name, args := range map[string][]any{
		"solana":            {"solana", "m/44'/60'/0'", 3, "xpub1", att, 0},
		"other path":        {"evm", "m/44'/60'/1'", 3, "xpub1", att, 0},
		"depth 4":           {"evm", "m/44'/60'/0'", 4, "xpub1", att, 0},
		"not an xpub":       {"evm", "m/44'/60'/0'", 3, "xprv1", att, 0},
		"short attestation": {"evm", "m/44'/60'/0'", 3, "xpub1", att[:63], 0},
		"negative counter":  {"evm", "m/44'/60'/0'", 3, "xpub1", att, -1},
	} {
		if _, err := conn.Exec(ctx, good, args...); err == nil {
			t.Errorf("%s accepted by the table constraints", name)
		}
	}
	if _, err := conn.Exec(ctx, good, "evm", "m/44'/60'/0'", 3, "xpub1", att, 50); err != nil {
		t.Fatal(err)
	}
	// down: the higher xpub counter goes back into wallet_seeds so a rollback cannot reuse an issued index
	if err := m.Migrate(6); err != nil {
		t.Fatalf("down: %v", err)
	}
	var next int
	if err := conn.QueryRow(ctx, `SELECT next_index FROM wallet_seeds WHERE seed_role = 'user_derivation'`).Scan(&next); err != nil || next != 50 {
		t.Fatalf("wallet_seeds.next_index after down = %d (%v), want 50", next, err)
	}
	if _, err := conn.Exec(ctx, `SELECT 1 FROM wallet_xpubs`); err == nil {
		t.Fatal("wallet_xpubs survived the down migration")
	}
	if err := m.Migrate(7); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func TestInsertXPub_NeverOverwrites(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, 2)
	row, _ := w.s.XPub(ctx, "evm")
	if err := w.s.InsertXPub(ctx, row); err != nil {
		t.Fatalf("re-inserting the identical xpub must be a no-op: %v", err)
	}
	other, _ := hdwallet.NewMnemonic()
	ox, _ := hdwallet.AccountXPub(other)
	row.XPub = ox
	if err := w.s.InsertXPub(ctx, row); !errors.Is(err, ErrXPubExists) {
		t.Fatalf("a different xpub overwrote the row: %v", err)
	}
}
