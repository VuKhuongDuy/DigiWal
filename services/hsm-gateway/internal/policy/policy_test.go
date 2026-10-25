package policy

import (
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/hsm-gateway/internal/intent"
)

const (
	hot    = "0x1111111111111111111111111111111111111111"
	warm   = "0x2222222222222222222222222222222222222222"
	token  = "0x3333333333333333333333333333333333333333"
	denied = "0x4444444444444444444444444444444444444444"
	user   = "0x5555555555555555555555555555555555555555"
)

func policyYAML(mut func(string) string) string {
	y := `
enabled: true
chain_id: 31337
hot_address: "` + hot + `"
warm_address: "` + warm + `"
denied_recipients: ["` + denied + `"]
fee_ceiling_wei: "50000000000"
rate_per_minute: 1000
native:
  max_per_tx: "1000000"
  treasury_max_per_tx: "50000000"
  window_1h: "2500000"
  window_24h: "4000000"
tokens:
  - symbol: TT
    contract: "` + token + `"
    max_per_tx: "1000"
    treasury_max_per_tx: "50000"
    window_1h: "2500"
    window_24h: "4000"
`
	if mut != nil {
		return mut(y)
	}
	return y
}

func mustPolicy(t *testing.T, mut func(string) string) *Policy {
	t.Helper()
	p, err := Parse([]byte(policyYAML(mut)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newEngine(t *testing.T, mut func(string) string) (*Engine, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return New(mustPolicy(t, mut), c.now), c
}

func A(s string) common.Address { return common.HexToAddress(s) }

// base is a valid native payout with a 1 wei fee (gas 21000 => worst-case fee 21000 wei).
func base(nonce uint64) *intent.Intent {
	return &intent.Intent{ChainID: 31337, Nonce: nonce, To: A(user), Value: big.NewInt(1000), Gas: 21000,
		MaxFee: big.NewInt(1), Tip: big.NewInt(1), Operation: intent.OpPayout, IdempotencyKey: "k"}
}

func pad32(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }

func erc20Data(sel string, recipient common.Address, amount int64) []byte {
	s, _ := hex.DecodeString(sel)
	return append(append(s, pad32(recipient)...), common.LeftPadBytes(big.NewInt(amount).Bytes(), 32)...)
}

func tokenTransfer(nonce uint64, recipient common.Address, amount int64) *intent.Intent {
	in := base(nonce)
	in.To, in.Value, in.Gas = A(token), new(big.Int), 60000
	in.Data = erc20Data("a9059cbb", recipient, amount)
	return in
}

func cancel(nonce uint64) *intent.Intent {
	in := base(nonce)
	in.To, in.Value, in.Operation = A(hot), new(big.Int), intent.OpCancel
	return in
}

func sweep(nonce uint64, to string, amount int64) *intent.Intent {
	in := base(nonce)
	in.To, in.Value, in.Operation = A(to), big.NewInt(amount), intent.OpTreasurySweep
	return in
}

// Every rule: one intent that is accepted and the mutation that makes it refused with exactly that rule id.
func TestEvaluate_EveryRuleRefusesAndTheBaseAccepts(t *testing.T) {
	type tc struct {
		name string
		mut  func(string) string // policy edit (optional)
		in   func() *intent.Intent
		want string // rule id; "" means accepted
	}
	badPad := func() *intent.Intent {
		in := tokenTransfer(1, A(user), 10)
		in.Data[5] = 1 // non-zero padding in the address word
		return in
	}
	cases := []tc{
		{"native payout is accepted", nil, func() *intent.Intent { return base(1) }, ""},
		{"erc20 transfer is accepted", nil, func() *intent.Intent { return tokenTransfer(1, A(user), 10) }, ""},
		{"treasury sweep to warm is accepted", nil, func() *intent.Intent { return sweep(1, warm, 40000000) }, ""},
		{"cancel is accepted", nil, func() *intent.Intent { return cancel(1) }, ""},

		{"kill switch", func(y string) string { return strings.Replace(y, "enabled: true", "enabled: false", 1) }, func() *intent.Intent { return base(1) }, RuleKillSwitch},
		{"chain pin", nil, func() *intent.Intent { in := base(1); in.ChainID = 1; return in }, RuleChainPin},
		{"reserved nonce 2^64-1", nil, func() *intent.Intent { return base(intent.MaxNonce) }, RuleReservedNonce},
		{"unknown operation", nil, func() *intent.Intent { in := base(1); in.Operation = "sweep"; return in }, RuleUnknownOperation},

		{"data shorter than a selector", nil, func() *intent.Intent { in := base(1); in.Data = []byte{1, 2}; return in }, RuleDataMalformed},
		{"transfer with wrong length", nil, func() *intent.Intent { in := tokenTransfer(1, A(user), 1); in.Data = in.Data[:67]; return in }, RuleDataMalformed},
		{"transfer with dirty address padding", nil, badPad, RuleDataMalformed},
		{"erc20 approve", nil, func() *intent.Intent {
			in := tokenTransfer(1, A(user), 1)
			in.Data = erc20Data("095ea7b3", A(user), 1)
			return in
		}, RuleApproveRefused},
		{"erc20 transferFrom", nil, func() *intent.Intent {
			in := tokenTransfer(1, A(user), 1)
			in.Data = erc20Data("23b872dd", A(user), 1)
			return in
		}, RuleSelectorRefused},
		{"arbitrary selector", nil, func() *intent.Intent { in := base(1); in.Data = erc20Data("deadbeef", A(user), 1); return in }, RuleSelectorRefused},
		{"transfer on a contract that is not an allowed asset", nil, func() *intent.Intent { in := tokenTransfer(1, A(user), 1); in.To = A(user); return in }, RuleAssetNotAllowed},
		{"erc20 transfer carrying value", nil, func() *intent.Intent { in := tokenTransfer(1, A(user), 1); in.Value = big.NewInt(1); return in }, RuleERC20ValueNonZero},

		{"native to zero address", nil, func() *intent.Intent { in := base(1); in.To = common.Address{}; return in }, RuleRecipientZero},
		{"erc20 to zero address", nil, func() *intent.Intent { return tokenTransfer(1, common.Address{}, 1) }, RuleRecipientZero},
		{"native to hot", nil, func() *intent.Intent { in := base(1); in.To = A(hot); return in }, RuleRecipientHot},
		{"erc20 to hot", nil, func() *intent.Intent { return tokenTransfer(1, A(hot), 1) }, RuleRecipientHot},
		{"native to the token contract", nil, func() *intent.Intent { in := base(1); in.To = A(token); return in }, RuleRecipientToken},
		{"erc20 to the token contract", nil, func() *intent.Intent { return tokenTransfer(1, A(token), 1) }, RuleRecipientToken},
		{"native to a denied recipient", nil, func() *intent.Intent { in := base(1); in.To = A(denied); return in }, RuleRecipientDenied},
		{"erc20 to a denied recipient", nil, func() *intent.Intent { return tokenTransfer(1, A(denied), 1) }, RuleRecipientDenied},

		{"payout to warm is not a payout", nil, func() *intent.Intent { in := base(1); in.To = A(warm); return in }, RuleClassMismatch},
		{"treasury sweep to someone else", nil, func() *intent.Intent { return sweep(1, user, 10) }, RuleClassMismatch},
		{"treasury sweep with no warm address", func(y string) string { return strings.Replace(y, `warm_address: "`+warm+`"`, "", 1) }, func() *intent.Intent { return sweep(1, warm, 10) }, RuleTreasuryDisabled},
		{"cancel with value", nil, func() *intent.Intent { in := cancel(1); in.Value = big.NewInt(1); return in }, RuleCancelShape},
		{"cancel with data", nil, func() *intent.Intent { in := cancel(1); in.Data = []byte{1, 2, 3, 4}; return in }, RuleSelectorRefused},
		{"cancel not to hot", nil, func() *intent.Intent { in := cancel(1); in.To = A(user); return in }, RuleCancelShape},

		{"native gas above cap", nil, func() *intent.Intent { in := base(1); in.Gas = 21001; return in }, RuleGasCap},
		{"erc20 gas above cap", nil, func() *intent.Intent { in := tokenTransfer(1, A(user), 1); in.Gas = 100001; return in }, RuleGasCap},
		{"max fee above the ceiling", nil, func() *intent.Intent { in := base(1); in.MaxFee = big.NewInt(50_000_000_001); return in }, RuleFeeCeiling},
		{"tip above max fee", nil, func() *intent.Intent { in := base(1); in.Tip = big.NewInt(2); return in }, RuleTipAboveFee},

		{"native above max_per_tx", nil, func() *intent.Intent { in := base(1); in.Value = big.NewInt(1_000_001); return in }, RuleMaxPerTx},
		{"erc20 above max_per_tx", nil, func() *intent.Intent { return tokenTransfer(1, A(user), 1001) }, RuleMaxPerTx},
		{"treasury sweep above its own cap", nil, func() *intent.Intent { return sweep(1, warm, 50_000_001) }, RuleTreasuryMaxPerTx},
		{"payout at max_per_tx is accepted", nil, func() *intent.Intent { in := base(1); in.Value = big.NewInt(1_000_000); return in }, ""},
		{"fee ceiling boundary is accepted", nil, func() *intent.Intent {
			in := base(1)
			in.MaxFee = big.NewInt(50_000_000_000)
			in.Tip = big.NewInt(1)
			return in
		}, RuleMaxPerTx /* fee 1.05e15 > window, see below */},
	}
	// The last case documents that a ceiling-priced fee blows the native window; recompute the want:
	cases[len(cases)-1].want = RuleWindow1h

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newEngine(t, c.mut)
			d := e.Evaluate(c.in())
			if c.want == "" {
				if !d.Allowed {
					t.Fatalf("refused with %s, want accepted", d.Rule)
				}
				return
			}
			if d.Allowed || d.Rule != c.want {
				t.Fatalf("allowed=%v rule=%q, want refused with %q", d.Allowed, d.Rule, c.want)
			}
		})
	}
}

func authorize(t *testing.T, e *Engine, in *intent.Intent) Decision {
	t.Helper()
	d, err := e.Authorize(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWindows_OneHourTwentyFourHourAndNativeFee(t *testing.T) {
	e, clk := newEngine(t, nil)
	// native: value + 21000 fee per payout; 1h cap 2_500_000 => two payouts of 1_000_000 fit (2_042_000), a third does not.
	for n := uint64(1); n <= 2; n++ {
		in := base(n)
		in.Value = big.NewInt(1_000_000)
		if d := authorize(t, e, in); !d.Allowed {
			t.Fatalf("payout %d refused: %s", n, d.Rule)
		}
	}
	in := base(3)
	in.Value = big.NewInt(1_000_000)
	if d := authorize(t, e, in); d.Allowed || d.Rule != RuleWindow1h {
		t.Fatalf("third payout: allowed=%v rule=%s, want window_1h", d.Allowed, d.Rule)
	}
	// 61 minutes later the 1 h window is empty but the 24 h window still holds 2.042M of 4M.
	clk.advance(61 * time.Minute)
	for n := uint64(3); n <= 4; n++ {
		in := base(n)
		in.Value = big.NewInt(1_000_000)
		d := authorize(t, e, in)
		if n == 3 && !d.Allowed {
			t.Fatalf("payout 3 after 61 min refused: %s", d.Rule)
		}
		if n == 4 && (d.Allowed || d.Rule != RuleWindow24h) {
			t.Fatalf("payout 4: allowed=%v rule=%s, want window_24h", d.Allowed, d.Rule)
		}
	}
	// 25 h after the start everything has aged out.
	clk.advance(25 * time.Hour)
	in = base(5)
	in.Value = big.NewInt(1_000_000)
	if d := authorize(t, e, in); !d.Allowed {
		t.Fatalf("after 25 h still refused: %s", d.Rule)
	}
}

func TestWindows_Erc20CountsTokenAndFeeInNativeWindow(t *testing.T) {
	e, _ := newEngine(t, nil)
	for n := uint64(1); n <= 2; n++ {
		if d := authorize(t, e, tokenTransfer(n, A(user), 1000)); !d.Allowed {
			t.Fatalf("token payout %d refused: %s", n, d.Rule)
		}
	}
	if d := authorize(t, e, tokenTransfer(3, A(user), 1000)); d.Allowed || d.Rule != RuleWindow1h {
		t.Fatalf("token window: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	// the fee of each token tx (60000 gas * 1 wei) went to the native window: 2 * 60000 used.
	e2, _ := newEngine(t, func(y string) string { return strings.Replace(y, `window_1h: "2500000"`, `window_1h: "100000"`, 1) })
	if d := authorize(t, e2, tokenTransfer(1, A(user), 1)); !d.Allowed {
		t.Fatal(d.Rule)
	}
	if d := authorize(t, e2, tokenTransfer(2, A(user), 1)); d.Allowed || d.Rule != RuleWindow1h {
		t.Fatalf("native fee window: allowed=%v rule=%s, want window_1h (fees 2*60000 > 100000)", d.Allowed, d.Rule)
	}
}

func TestTreasurySweepDoesNotConsumePayoutWindow(t *testing.T) {
	e, _ := newEngine(t, nil)
	for n := uint64(1); n <= 5; n++ { // 5 x 40M would blow any payout window
		if d := authorize(t, e, sweep(n, warm, 40_000_000)); !d.Allowed || d.Rule != AcceptTreasurySweep {
			t.Fatalf("sweep %d: allowed=%v rule=%s", n, d.Allowed, d.Rule)
		}
	}
	in := base(6)
	in.Value = big.NewInt(1_000_000)
	if d := authorize(t, e, in); !d.Allowed {
		t.Fatalf("payout after sweeps refused: %s", d.Rule)
	}
}

func TestReplacement_FeeBumpCancelAndRetry(t *testing.T) {
	e, _ := newEngine(t, nil)
	orig := base(7)
	orig.Value = big.NewInt(1_000_000)
	orig.MaxFee, orig.Tip = big.NewInt(10), big.NewInt(5)
	if d := authorize(t, e, orig); !d.Allowed {
		t.Fatal(d.Rule)
	}

	// identical retry (lost response): accepted, counted once
	retry := *orig
	if d := authorize(t, e, &retry); !d.Allowed || !d.Replacement {
		t.Fatalf("retry: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	// fee bump: same body, higher fees
	bump := *orig
	bump.MaxFee, bump.Tip = big.NewInt(20), big.NewInt(9)
	if d := authorize(t, e, &bump); !d.Allowed || d.Rule != AcceptFeeBump {
		t.Fatalf("bump: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	// window accounting is per nonce at the maximum, not per signature: a second payout still fits
	// (3 versions of nonce 7 count once: 1_000_000 + max fee 21000*20, so 1_420_000 of 2_500_000).
	second := base(8)
	second.Value = big.NewInt(1_000_000)
	if d := authorize(t, e, second); !d.Allowed {
		t.Fatalf("second payout after replacements refused: %s (window double-counted the nonce)", d.Rule)
	}
	// not a bump: lower fee, changed value, changed recipient, changed gas
	for name, mut := range map[string]func(*intent.Intent){
		"lower max fee": func(i *intent.Intent) { i.MaxFee = big.NewInt(19) },
		"lower tip":     func(i *intent.Intent) { i.Tip = big.NewInt(8) },
		"other value":   func(i *intent.Intent) { i.Value = big.NewInt(1) },
		"other to":      func(i *intent.Intent) { i.To = A("0x6666666666666666666666666666666666666666") },
		"other gas":     func(i *intent.Intent) { i.Gas = 21000 - 1 },
	} {
		x := bump
		mut(&x)
		if d := authorize(t, e, &x); d.Allowed || d.Rule != RuleReplacementNotBump {
			t.Fatalf("%s: allowed=%v rule=%s, want replacement_not_bump", name, d.Allowed, d.Rule)
		}
	}
	// a bump above the ceiling is refused by the ceiling
	hi := bump
	hi.MaxFee = big.NewInt(50_000_000_001)
	if d := authorize(t, e, &hi); d.Allowed || d.Rule != RuleFeeCeiling {
		t.Fatalf("bump over ceiling: %s", d.Rule)
	}
	// cancel of that nonce (any value payout -> cancel): accepted, class cancel
	c := cancel(7)
	if d := authorize(t, e, c); !d.Allowed || d.Rule != AcceptCancel || !d.Replacement {
		t.Fatalf("cancel of a seen nonce: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	// and cancel works on a nonce never seen
	if d := authorize(t, e, cancel(99)); !d.Allowed {
		t.Fatalf("cancel on a fresh nonce: %s", d.Rule)
	}
}

func TestRateLimit(t *testing.T) {
	e, clk := newEngine(t, func(y string) string { return strings.Replace(y, "rate_per_minute: 1000", "rate_per_minute: 3", 1) })
	for n := uint64(1); n <= 3; n++ {
		if d := authorize(t, e, base(n)); !d.Allowed {
			t.Fatalf("%d: %s", n, d.Rule)
		}
	}
	if d := authorize(t, e, base(4)); d.Allowed || d.Rule != RuleRateLimit {
		t.Fatalf("4th in a minute: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	clk.advance(61 * time.Second)
	if d := authorize(t, e, base(4)); !d.Allowed {
		t.Fatalf("after a minute: %s", d.Rule)
	}
}

// A failed audit append means nothing is committed: no window use, no nonce memory.
func TestAuthorize_PersistFailureCommitsNothing(t *testing.T) {
	e, _ := newEngine(t, nil)
	in := base(1)
	in.Value = big.NewInt(1_000_000)
	if _, err := e.Authorize(in, func(Decision) error { return os.ErrClosed }); err == nil {
		t.Fatal("persist error swallowed")
	}
	for n := uint64(1); n <= 2; n++ { // both still fit, so the failed one was not counted
		x := base(n + 10)
		x.Value = big.NewInt(1_000_000)
		if d := authorize(t, e, x); !d.Allowed {
			t.Fatalf("payout refused after a failed append: %s", d.Rule)
		}
	}
}

func TestRestore_RebuildsWindowsAndNonces(t *testing.T) {
	e, clk := newEngine(t, nil)
	in := base(1)
	in.Value = big.NewInt(1_000_000)
	d := authorize(t, e, in)
	rec := recordOf(in, d, clk.now())

	e2, _ := New(mustPolicy(t, nil), clk.now), 0
	e2.Restore(rec)
	in2 := base(2)
	in2.Value = big.NewInt(1_000_000)
	authorize(t, e2, in2)
	in3 := base(3)
	in3.Value = big.NewInt(1_000_000)
	if d := authorize(t, e2, in3); d.Allowed || d.Rule != RuleWindow1h {
		t.Fatalf("restored engine forgot the first payout: allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	// and the nonce memory came back: a different body for nonce 1 is not a fee bump
	other := *in
	other.Value = big.NewInt(5)
	if d := e2.Evaluate(&other); d.Rule != RuleReplacementNotBump {
		t.Fatalf("restored engine forgot nonce 1: %s", d.Rule)
	}
}

// ---- policy file

func TestParse_FailsClosedOnMissingOrBadFields(t *testing.T) {
	for name, mut := range map[string]func(string) string{
		"enabled missing": func(y string) string { return strings.Replace(y, "enabled: true\n", "", 1) },
		"no chain":        func(y string) string { return strings.Replace(y, "chain_id: 31337", "chain_id: 0", 1) },
		"bad hot":         func(y string) string { return strings.Replace(y, hot, "0x12", 1) },
		"zero hot": func(y string) string {
			return strings.Replace(y, `hot_address: "`+hot+`"`, `hot_address: "0x0000000000000000000000000000000000000000"`, 1)
		},
		"warm equals hot":        func(y string) string { return strings.Replace(y, warm, hot, 1) },
		"native cap missing":     func(y string) string { return strings.Replace(y, `  max_per_tx: "1000000"`+"\n", "", 1) },
		"window not a number":    func(y string) string { return strings.Replace(y, `window_24h: "4000000"`, `window_24h: "lots"`, 1) },
		"unknown key":            func(y string) string { return y + "bypass: true\n" },
		"token without contract": func(y string) string { return strings.Replace(y, `contract: "`+token+`"`, `contract: ""`, 1) },
		"fee ceiling zero":       func(y string) string { return strings.Replace(y, `"50000000000"`, `"0"`, 1) },
	} {
		if _, err := Parse([]byte(policyYAML(mut))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := mustPolicy(t, func(y string) string { return strings.Replace(y, "fee_ceiling_wei: \"50000000000\"\n", "", 1) })
	if p.FeeCeiling.Int64() != 50_000_000_000 || p.Native.GasCap != 21000 || p.Tokens[A(token)].GasCap != 100000 {
		t.Fatalf("defaults wrong: ceiling %v nativeGas %d tokenGas %d", p.FeeCeiling, p.Native.GasCap, p.Tokens[A(token)].GasCap)
	}
}

func TestLoadFile_HashChangesWithEditsAndPermsAreChecked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(policyYAML(nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	p1, err := LoadFile(path, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p1b, _ := LoadFile(path, LoadOptions{})
	if p1.Hash != p1b.Hash || len(p1.Hash) != 64 {
		t.Fatal("hash is not stable")
	}
	edited := strings.Replace(policyYAML(nil), `max_per_tx: "1000000"`, `max_per_tx: "1000001"`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := LoadFile(path, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p1.Hash == p2.Hash {
		t.Fatal("editing the policy file did not change its hash")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path, LoadOptions{}); err == nil {
		t.Fatal("a world-writable policy file was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if _, err := LoadFile(path, LoadOptions{RequireRootOwner: true}); err == nil {
			t.Fatal("a file not owned by root was accepted with RequireRootOwner")
		}
	}
}
