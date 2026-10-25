package audit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"

	"digiwal/services/sweep-signer/internal/derive"
	"digiwal/services/sweep-signer/internal/store"
	"digiwal/services/sweep-signer/internal/vault"
)

const (
	mnemonic = "legal winner thank year wave sausage worth useful legal winner thank yellow"
)

type fakeDir struct {
	mu     sync.Mutex
	rows   []store.Issued
	xpub   string
	dirErr error
}

func (d *fakeDir) IssuedAfter(_ context.Context, _ string, after int64, limit int) ([]store.Issued, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirErr != nil {
		return nil, d.dirErr
	}
	var out []store.Issued
	for _, r := range d.rows {
		if int64(r.Index) > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *fakeDir) AccountXPub(context.Context) (string, error) {
	if d.xpub == "" {
		return "", store.ErrNotFound
	}
	return d.xpub, nil
}

// fakeGW is a TEST-ONLY stand-in for hsm-gateway's unseal (a _test file: the service has no software unseal path).
type fakeGW struct {
	seed  []byte
	err   error
	calls int
}

type rowSrc struct{}

func (rowSrc) ActiveSecret(context.Context) (vault.Row, error) {
	return vault.Row{Role: "user_derivation", Purpose: "sweep", Version: 1, KEKVersion: 1}, nil
}

func (f *fakeGW) UnsealInto(_ context.Context, _ vault.Row, dst []byte) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return copy(dst, f.seed), nil
}

func seedFor(t *testing.T, mn string) []byte {
	t.Helper()
	s, err := hdw.NewSeedFromMnemonic(mn)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func provider(t *testing.T, seed []byte) *vault.Provider {
	t.Helper()
	return &vault.Provider{Source: rowSrc{}, Unsealer: &fakeGW{seed: seed}}
}

func issue(t *testing.T, seed []byte, n int) []store.Issued {
	t.Helper()
	var rows []store.Issued
	for i := 0; i < n; i++ {
		a, err := derive.AddressAt(seed, uint32(i))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, store.Issued{UserID: fmt.Sprintf("u%d", i), Index: uint32(i), Address: a.Hex()})
	}
	return rows
}

func newAuditor(t *testing.T, d *fakeDir, seed []byte) (*Auditor, *[]string) {
	t.Helper()
	a := New(d, provider(t, seed))
	a.Batch, a.Window = 4, 3
	var alerts []string
	a.alert = func(m string) { alerts = append(alerts, m) }
	return a, &alerts
}

func TestAudit_SigningIsClosedUntilTheFirstCleanPass(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 10)}
	a, alerts := newAuditor(t, d, seed)
	if a.Ready() == nil {
		t.Fatal("ready before any pass")
	}
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Ready(); err != nil {
		t.Fatalf("not ready after a clean pass: %v", err)
	}
	if a.Checked() != 10 || len(*alerts) != 0 || a.Frozen() != "" {
		t.Fatalf("checked %d alerts %v frozen %q", a.Checked(), *alerts, a.Frozen())
	}
}

func TestAudit_AMismatchedRowFreezesAllSigningAndAlerts(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 10)}
	d.rows[6].Address = "0x000000000000000000000000000000000000dEaD" // an address an attacker-derived xpub issued
	a, alerts := newAuditor(t, d, seed)
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Ready() == nil || a.Frozen() == "" || a.Mismatches() != 1 {
		t.Fatalf("not frozen: ready=%v frozen=%q mismatches=%d", a.Ready(), a.Frozen(), a.Mismatches())
	}
	if len(*alerts) != 1 {
		t.Fatalf("alerts: %v", *alerts)
	}
	// stays frozen: a later pass over honest data does not reopen it
	d.rows = issue(t, seed, 10)
	_ = a.RunOnce(context.Background())
	if a.Ready() == nil {
		t.Fatal("a freeze must last until restart")
	}
}

// The attack of invariant 11: the xpub in the database is another seed's. The Seed's own account xpub differs, and so does
// every address issued from it.
func TestAudit_ASwappedXPubIsDetected(t *testing.T) {
	seed := seedFor(t, mnemonic)
	attacker := seedFor(t, "test test test test test test test test test test test junk")
	attackerXPub, _ := derive.AccountXPub(attacker)

	// (a) only the xpub row is swapped, no address has been issued from it yet
	d := &fakeDir{xpub: attackerXPub}
	a, _ := newAuditor(t, d, seed)
	_ = a.RunOnce(context.Background())
	if a.Ready() == nil {
		t.Fatal("a swapped xpub row was not detected before any address was issued from it")
	}

	// (b) the addresses were issued from the attacker's xpub (what wallet-service would have done)
	d2 := &fakeDir{rows: issue(t, attacker, 3)}
	a2, _ := newAuditor(t, d2, seed)
	_ = a2.RunOnce(context.Background())
	if a2.Ready() == nil || a2.Mismatches() != 1 {
		t.Fatal("addresses issued from another seed's xpub were not detected")
	}

	// honest xpub passes
	good, _ := derive.AccountXPub(seed)
	d3 := &fakeDir{xpub: good, rows: issue(t, seed, 3)}
	a3, _ := newAuditor(t, d3, seed)
	if err := a3.RunOnce(context.Background()); err != nil || a3.Ready() != nil {
		t.Fatalf("honest xpub refused: %v %v", err, a3.Ready())
	}
	// the account key really is depth 3 public (sanity of the fixture)
	k, err := hdkeychain.NewKeyFromString(good)
	if err != nil || k.Depth() != 3 || !k.IsForNet(&chaincfg.MainNetParams) {
		t.Fatal("fixture xpub is not an account xpub")
	}
}

func TestAudit_NewRowsAreCheckedOnTheNextPassAndOldOnesRolling(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 5)}
	a, _ := newAuditor(t, d, seed)
	_ = a.RunOnce(context.Background())
	first := a.Checked() // 5 fresh, nothing older yet

	d.rows = issue(t, seed, 9) // four new users registered
	_ = a.RunOnce(context.Background())
	if a.Checked() <= first+4 {
		t.Fatalf("the four new rows and a rolling window of older ones should have been checked: %d -> %d", first, a.Checked())
	}
	// a new row that is wrong is caught on the very next pass
	d.rows = append(d.rows, store.Issued{UserID: "evil", Index: 9, Address: "0x000000000000000000000000000000000000dEaD"})
	_ = a.RunOnce(context.Background())
	if a.Ready() == nil {
		t.Fatal("a bad new row was not caught on the next pass")
	}
}

// An old row that goes bad later (the database is edited) is caught by the rolling window within a few passes.
func TestAudit_ARowEditedLaterIsCaughtByTheRollingWindow(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 8)}
	a, _ := newAuditor(t, d, seed)
	_ = a.RunOnce(context.Background())
	d.rows[2].Address = "0x000000000000000000000000000000000000dEaD"
	for i := 0; i < 6 && a.Ready() == nil; i++ {
		_ = a.RunOnce(context.Background())
	}
	if a.Ready() == nil {
		t.Fatal("an edited old row was never re-derived")
	}
}

// Infrastructure trouble is not a verdict: the database down or the Seed not openable neither freezes nor opens the gate.
func TestAudit_InfrastructureFailureIsNotAMismatch(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 3), dirErr: errors.New("database down")}
	a, alerts := newAuditor(t, d, seed)
	if err := a.RunOnce(context.Background()); err == nil {
		t.Fatal("a database failure returned no error")
	}
	if a.Frozen() != "" || len(*alerts) != 0 || a.Ready() == nil {
		t.Fatalf("frozen=%q alerts=%v ready=%v", a.Frozen(), *alerts, a.Ready())
	}
	// the gateway refuses (or is down): the Seed cannot be opened, so no verdict either
	d.dirErr = nil
	a.seeds = &vault.Provider{Source: rowSrc{}, Unsealer: &fakeGW{err: vault.ErrAuthFailed}}
	if err := a.RunOnce(context.Background()); err == nil || a.Frozen() != "" {
		t.Fatalf("an unopenable seed: err=%v frozen=%q", err, a.Frozen())
	}
}

// The audit opens the Seed once per pass (not once per row), and a pass with nothing to check still verifies the xpub.
func TestAudit_OneUnsealPerPass(t *testing.T) {
	seed := seedFor(t, mnemonic)
	d := &fakeDir{rows: issue(t, seed, 12)}
	a, _ := newAuditor(t, d, seed)
	_ = a.RunOnce(context.Background())
	if n := a.seeds.(*vault.Provider).Unseals(); n != 1 {
		t.Fatalf("%d unseals for one pass over 12 rows", n)
	}
}
