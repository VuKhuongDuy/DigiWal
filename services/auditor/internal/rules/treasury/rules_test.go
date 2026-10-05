package treasury

import (
	"math/big"
	"testing"

	"digiwal/services/auditor/internal/finding"
)

const (
	hot  = "0x00000000000000000000000000000000000000a1"
	warm = "0x00000000000000000000000000000000000000a2"
	cold = "0x00000000000000000000000000000000000000a3"
	dep  = "0x00000000000000000000000000000000000000d1"
	ext  = "0x00000000000000000000000000000000000000e1"
)

func TestTierOfAndClassify(t *testing.T) {
	a := Addresses{Hot: hot, Warm: warm, Cold: cold}
	isDep := func(s string) bool { return s == dep }
	for addr, want := range map[string]string{hot: Hot, warm: Warm, cold: Cold, dep: Deposit, ext: External, "": External,
		"0x00000000000000000000000000000000000000A1": Hot} {
		if got := a.TierOf(addr, isDep); got != want {
			t.Errorf("TierOf(%s) = %s, want %s", addr, got, want)
		}
	}
	if got := (Addresses{Hot: hot, Warm: warm}).TierOf(cold, nil); got != External {
		t.Errorf("without a Cold address nothing is Cold: %s", got)
	}
	for _, c := range []struct{ from, to, kind string }{
		{Hot, Warm, "hot_to_warm"}, {Warm, Hot, "warm_to_hot"}, {Warm, Cold, "warm_to_cold"}, {Cold, Warm, "cold_to_warm"},
		{Hot, Cold, "hot_to_cold"}, {Cold, Hot, "cold_to_hot"}, {Deposit, Hot, "sweep"}, {Hot, Deposit, "gas_topup"},
		{Hot, External, "withdrawal"}, {External, Hot, "deposit_into_tier"}, {External, Warm, "deposit_into_tier"},
		{Deposit, Cold, "deposit_into_tier"}, {Warm, External, "unexplained"},
	} {
		if got := Classify(c.from, c.to); got != c.kind {
			t.Errorf("Classify(%s,%s) = %s, want %s", c.from, c.to, got, c.kind)
		}
	}
}

func rule(v *finding.Violation) string {
	if v == nil {
		return ""
	}
	return v.Rule
}

func TestT1(t *testing.T) {
	if rule(T1(5, 5)) != "" || rule(T1(3, 5)) != "" {
		t.Error("a chain count at or below next_nonce is fine")
	}
	if v := T1(6, 5); rule(v) != "T1" || !v.Immediate || v.Severity != finding.Critical {
		t.Errorf("T1(6,5) = %+v", v)
	}
}

func TestCheckHotTx(t *testing.T) {
	k := Known{Hashes: map[string]bool{"0xaa": true, "0xbb": true}, Nonces: map[uint64]bool{0: true, 1: true}}
	cases := []struct {
		name string
		h    HotTx
		want string
	}{
		{"original", HotTx{Hash: "0xaa", Nonce: 0}, ""},
		{"a stored Fee Bump, any case", HotTx{Hash: "0xBB", Nonce: 0}, ""},
		{"another tx took a known nonce", HotTx{Hash: "0xcc", Nonce: 1}, "T2"},
		{"unknown nonce, value left", HotTx{Hash: "0xcc", Nonce: 9, HasMovement: true}, "T4"},
		{"unknown nonce, no value", HotTx{Hash: "0xcc", Nonce: 9}, "T2"},
	}
	for _, c := range cases {
		v := CheckHotTx(c.h, k)
		if rule(v) != c.want || (v != nil && (!v.Immediate || v.Severity != finding.Critical)) {
			t.Errorf("%s: %+v, want %q", c.name, v, c.want)
		}
	}
}

func TestT3(t *testing.T) {
	cases := []struct {
		name string
		s    Signature
		want string
	}{
		{"withdrawal", Signature{Key: "withdrawal:1", CallerCN: CNWithdrawalService, WithdrawalExists: true}, ""},
		{"withdrawal, no request", Signature{Key: "withdrawal:1", CallerCN: CNWithdrawalService}, "T3"},
		{"withdrawal signed for the sweeper", Signature{Key: "withdrawal:1", CallerCN: CNSweeper, WithdrawalExists: true}, "T3"},
		{"gas top-up by the sweeper", Signature{Key: "gas-topup:31337:0xab:0", CallerCN: CNSweeper}, ""},
		{"gas top-up by withdrawal-service", Signature{Key: "gas-topup:31337:0xab:0", CallerCN: CNWithdrawalService}, "T3"},
		{"sweep", Signature{Key: "sweep:x", CallerCN: CNSweeper}, ""},
		{"treasury by either", Signature{Key: "treasury:x", CallerCN: CNWithdrawalService}, ""},
		{"unknown purpose", Signature{Key: "mystery:1", CallerCN: CNSweeper}, "T3"},
		{"unknown caller", Signature{Key: "gas-topup:1", CallerCN: "chain-watcher"}, "T3"},
	}
	for _, c := range cases {
		if got := rule(T3(c.s)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestT5(t *testing.T) {
	mv := func(from, to string) Movement {
		return Movement{FromTier: from, ToTier: to, Amount: big.NewInt(1), Kind: Classify(from, to)}
	}
	for _, c := range []struct {
		m    Movement
		want string
	}{
		{mv(Warm, Hot), finding.Info}, {mv(Warm, Cold), finding.Info}, {mv(Cold, Warm), finding.Info},
		{mv(Warm, External), finding.Warn}, {mv(Cold, External), finding.Warn}, {mv(Cold, Deposit), finding.Warn},
		{mv(Hot, Warm), ""}, {mv(Hot, External), ""}, {mv(External, Warm), ""},
	} {
		v := T5(c.m)
		got := ""
		if v != nil {
			got = v.Severity
		}
		if got != c.want {
			t.Errorf("T5(%s->%s) severity %q, want %q", c.m.FromTier, c.m.ToTier, got, c.want)
		}
	}
}
