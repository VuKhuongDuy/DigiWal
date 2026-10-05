package custody

import (
	"math/big"
	"testing"

	"digiwal/services/auditor/internal/finding"
)

func n(v int64) *big.Int { return big.NewInt(v) }

// base is a token with one user who deposited 500, nothing else.
func base() Inputs {
	return Inputs{AssetID: "a", Symbol: "TKN", Block: 10, OnChain: n(500), HotOnChain: n(500), ChainAccount: n(-500), UserTotal: n(500),
		ActiveLocks: n(0), ObservedUpToB: n(500), Credited: n(500), UnsettledWithdrawn: n(0), UnpostedGas: n(0), Capital: n(0), Tolerance: n(0)}
}

func rules(in Inputs) map[string]string {
	out := map[string]string{}
	for _, v := range Evaluate(Compute(in)) {
		out[v.Rule] = v.Severity
	}
	return out
}

func TestResidualIsZeroWhenEverythingIsExplained(t *testing.T) {
	cases := map[string]func(*Inputs){
		"a deposit credited": func(*Inputs) {},
		"a deposit not credited yet (lag)": func(in *Inputs) {
			in.OnChain, in.HotOnChain, in.ObservedUpToB, in.Credited, in.ChainAccount, in.UserTotal = n(900), n(900), n(900), n(500), n(-500), n(500)
		},
		"a Flagged Deposit held": func(in *Inputs) {
			in.OnChain, in.ObservedUpToB, in.FlaggedHeld = n(800), n(800), n(300)
		},
		"a withdrawal of 100 mined, not settled": func(in *Inputs) {
			in.OnChain, in.HotOnChain, in.UnsettledWithdrawn = n(400), n(400), n(100)
		},
		"the same withdrawal once settled": func(in *Inputs) {
			in.OnChain, in.HotOnChain, in.ChainAccount, in.UserTotal = n(400), n(400), n(-400), n(400)
		},
		"gas paid, not posted yet (native)": func(in *Inputs) {
			in.OnChain, in.HotOnChain, in.UnpostedGas, in.Capital = n(579), n(579), n(21), n(100) // a 100 float paid 21 gas
		},
		"operator capital in the Hot Wallet": func(in *Inputs) {
			in.OnChain, in.HotOnChain, in.Capital = n(10500), n(10500), n(10000)
		},
		"everything at once": func(in *Inputs) {
			// capital 10000, user 500 credited, 200 deposit observed not credited, 100 withdrawal mined not settled, 21 gas unposted
			in.OnChain, in.HotOnChain = n(10000+500+200-100-21), n(10000+500+200-100-21)
			in.ObservedUpToB, in.Credited, in.UnsettledWithdrawn, in.UnpostedGas, in.Capital = n(700), n(500), n(100), n(21), n(10000)
		},
	}
	for name, mut := range cases {
		in := base()
		mut(&in)
		if r := Compute(in); r.Residual.Sign() != 0 {
			t.Errorf("%s: residual %s, want 0", name, r.Residual)
		}
		if got := rules(in); len(got) != 0 {
			t.Errorf("%s: findings %v", name, got)
		}
	}
}

func TestResidualSeverity(t *testing.T) {
	in := base()
	in.OnChain, in.HotOnChain = n(499), n(499) // 1 unit short, tolerance 0
	if got := rules(in); got["C1"] != finding.Critical {
		t.Errorf("a shortfall beyond a zero tolerance is critical: %v", got)
	}
	in.Tolerance = n(5)
	if got := rules(in); got["C1"] != finding.Warn {
		t.Errorf("within the tolerance it is a warn: %v", got)
	}
	in.OnChain = n(490) // 10 short, tolerance 5
	if got := rules(in); got["C1"] != finding.Critical {
		t.Errorf("beyond the tolerance: %v", got)
	}
	in = base()
	in.OnChain = n(600) // surplus is also a residual: funds nobody accounts for
	if got := rules(in); got["C1"] != finding.Critical {
		t.Errorf("a surplus is reported too: %v", got)
	}
	for _, v := range Evaluate(Compute(in)) {
		if v.Immediate {
			t.Errorf("reconciliation compares stores read a moment apart, never Immediate: %+v", v)
		}
	}
}

func TestSolvencyAndHotCoverage(t *testing.T) {
	in := base()
	in.UserTotal = n(501) // the ledger owes users 501, the chain holds 500
	if got := rules(in); got["C2"] != finding.Critical {
		t.Errorf("C2: %v", got)
	}
	in = base()
	in.ActiveLocks, in.HotOnChain = n(300), n(200) // 300 reserved for withdrawal, Hot holds 200 (rest in Warm, say)
	in.OnChain = n(500)
	if got := rules(in); got["C3"] != finding.Warn || len(got) != 1 {
		t.Errorf("C3: %v", got)
	}
}

// Without any operator capital, gas comes out of what users are owed: that is insolvency, and C2 says so.
func TestGasWithNoFloatIsInsolvency(t *testing.T) {
	in := base()
	in.OnChain, in.HotOnChain, in.UnpostedGas = n(479), n(479), n(21)
	if got := rules(in); got["C2"] != finding.Critical || got["C1"] != "" {
		t.Errorf("the books balance (C1 quiet) but users are short: %v", got)
	}
}

func TestNilsAreZero(t *testing.T) {
	if r := Compute(Inputs{}); r.Residual.Sign() != 0 {
		t.Errorf("an asset with no figures reconciles to zero, got %s", r.Residual)
	}
}
