package validate

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/registry"
	"digiwal/services/withdrawal-service/internal/screening"
	"digiwal/services/withdrawal-service/internal/store"
)

const (
	eth        = "11111111-1111-1111-1111-111111111111"
	good       = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	pendingTo  = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
	screenedTo = "0x90F79bf6EB2c4f870365E785982E1f101E93b906"
)

var minAmt = big.NewInt(600_000_000_000_000)

type fakeWL struct {
	entries map[string]string // EIP-55 address -> status, for user "alice"
	err     error
	calls   int
}

func (f *fakeWL) WhitelistStatus(_ context.Context, user, addr string, _ time.Duration) (string, bool, error) {
	f.calls++
	if f.err != nil {
		return "", false, f.err
	}
	if user != "alice" {
		return "", false, nil
	}
	st, ok := f.entries[addr]
	return st, ok, nil
}

type fakeDest struct {
	reason string
	err    error
	calls  int
}

func (d *fakeDest) Refuse(context.Context, common.Address) (string, error) {
	d.calls++
	return d.reason, d.err
}

func newValidator(dest Destinations) (*Validator, *fakeWL) {
	wl := &fakeWL{entries: map[string]string{
		good: store.WhitelistActive, pendingTo: store.WhitelistPending, screenedTo: store.WhitelistActive,
	}}
	return &Validator{
		Assets:       registry.New(registry.Asset{ID: eth, Type: "native", Symbol: "ETH", Decimals: 18, MinWithdrawalAmount: minAmt}),
		Whitelist:    wl,
		Screening:    screening.New(screenedTo),
		Cooldown:     24 * time.Hour,
		Destinations: dest,
	}, wl
}

func wd(user, asset, to string, amount *big.Int) store.Withdrawal {
	return store.Withdrawal{ID: "w1", UserID: user, AssetID: asset, ToAddress: to, Amount: amount}
}

func TestEachRule(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 256)
	cases := []struct {
		name string
		w    store.Withdrawal
		want string
	}{
		{"all good", wd("alice", eth, good, minAmt), ""},
		{"lower-case address is the same entry", wd("alice", eth, "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", minAmt), ""},
		{"asset id is case-insensitive", wd("alice", "11111111-1111-1111-1111-111111111111", good, minAmt), ""},
		{"unknown asset", wd("alice", "22222222-2222-2222-2222-222222222222", good, minAmt), AssetNotRegistered},
		{"malformed address", wd("alice", eth, "0x1234", minAmt), InvalidToAddress},
		{"zero address", wd("alice", eth, "0x0000000000000000000000000000000000000000", minAmt), InvalidToAddress},
		{"one below the minimum", wd("alice", eth, good, new(big.Int).Sub(minAmt, big.NewInt(1))), BelowMinimum},
		{"above uint256", wd("alice", eth, good, huge), AmountTooLarge},
		{"not whitelisted at all", wd("alice", eth, "0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65", minAmt), NotWhitelisted},
		{"whitelisted but still pending", wd("alice", eth, pendingTo, minAmt), NotWhitelisted},
		{"another user's whitelist doesn't count", wd("bob", eth, good, minAmt), NotWhitelisted},
		{"on the screening list", wd("alice", eth, screenedTo, minAmt), OnScreeningList},
	}
	for _, c := range cases {
		v, _ := newValidator(nil)
		got, err := v.Check(context.Background(), c.w)
		if err != nil || got != c.want {
			t.Errorf("%s: %q (%v), want %q", c.name, got, err, c.want)
		}
	}
}

// The decided order: min amount is reported before the whitelist (card AC), and nothing
// remote is asked once a local rule has already failed.
func TestOrder(t *testing.T) {
	dest := &fakeDest{reason: DestinationIsDeposit}
	v, wl := newValidator(dest)
	// Below minimum AND not whitelisted AND would be refused remotely: min wins, nothing else asked.
	got, _ := v.Check(context.Background(), wd("alice", eth, "0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65", big.NewInt(1)))
	if got != BelowMinimum || wl.calls != 0 || dest.calls != 0 {
		t.Fatalf("got %q (whitelist calls %d, destination calls %d), want %q and no further checks", got, wl.calls, dest.calls, BelowMinimum)
	}
	// Screened AND would be refused remotely: screening wins, remote never asked.
	got, _ = v.Check(context.Background(), wd("alice", eth, screenedTo, minAmt))
	if got != OnScreeningList || dest.calls != 0 {
		t.Fatalf("got %q (destination calls %d), want %q before the remote check", got, dest.calls, OnScreeningList)
	}
	// Everything local passes: the destination check decides.
	got, _ = v.Check(context.Background(), wd("alice", eth, good, minAmt))
	if got != DestinationIsDeposit || dest.calls != 1 {
		t.Fatalf("got %q (destination calls %d), want %q", got, dest.calls, DestinationIsDeposit)
	}
}

// No verdict is an error, never a pass: a failing dependency must not let a request through.
func TestFailuresAreNotVerdicts(t *testing.T) {
	v, wl := newValidator(nil)
	wl.err = errors.New("db down")
	if got, err := v.Check(context.Background(), wd("alice", eth, good, minAmt)); err == nil || got != "" {
		t.Fatalf("whitelist failure: %q %v, want an error", got, err)
	}
	v, _ = newValidator(&fakeDest{err: errors.New("wallet-service down")})
	if got, err := v.Check(context.Background(), wd("alice", eth, good, minAmt)); err == nil || got != "" {
		t.Fatalf("destination failure: %q %v, want an error", got, err)
	}
}
