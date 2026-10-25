package policy

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/hsm-gateway/internal/intent"
)

// Rule ids. A refusal carries exactly one; an acceptance carries an Accept* id.
const (
	RuleKillSwitch         = "kill_switch"
	RuleRateLimit          = "rate_limit"
	RuleChainPin           = "chain_pin"
	RuleReservedNonce      = "reserved_nonce"
	RuleUnknownOperation   = "unknown_operation"
	RuleDataMalformed      = "data_malformed"
	RuleApproveRefused     = "erc20_approve_refused"
	RuleSelectorRefused    = "data_selector_refused"
	RuleERC20ValueNonZero  = "erc20_value_nonzero"
	RuleAssetNotAllowed    = "asset_not_allowed"
	RuleRecipientZero      = "recipient_zero"
	RuleRecipientHot       = "recipient_hot"
	RuleRecipientToken     = "recipient_token_contract"
	RuleRecipientDenied    = "recipient_denied"
	RuleClassMismatch      = "class_mismatch"
	RuleTreasuryDisabled   = "treasury_not_configured"
	RuleCancelShape        = "cancel_shape"
	RuleGasCap             = "gas_cap"
	RuleFeeCeiling         = "fee_ceiling"
	RuleTipAboveFee        = "tip_above_max_fee"
	RuleMaxPerTx           = "max_per_tx"
	RuleTreasuryMaxPerTx   = "treasury_max_per_tx"
	RuleWindow1h           = "window_1h"
	RuleWindow24h          = "window_24h"
	RuleReplacementNotBump = "replacement_not_bump"

	AcceptPayout        = "accept_payout"
	AcceptTreasurySweep = "accept_treasury_sweep"
	AcceptCancel        = "accept_cancel"
	AcceptFeeBump       = "accept_replacement_fee_bump"
)

var (
	selTransfer = []byte{0xa9, 0x05, 0x9c, 0xbb}
	selApprove  = []byte{0x09, 0x5e, 0xa7, 0xb3}

	window1h  = time.Hour
	window24h = 24 * time.Hour
	// retention bounds the nonce memory: the widest window.
	retention = window24h
)

// Decision is the engine's verdict plus everything the audit log records about it.
type Decision struct {
	Allowed     bool
	Rule        string
	Class       string
	Asset       string // NativeKey or lower-case contract
	Recipient   common.Address
	Selector    string   // 8 hex chars, "" for a native transfer
	Amount      *big.Int // asset units moved (value for native, ERC-20 amount for tokens)
	FeeWei      *big.Int // worst case gas*max_fee
	Replacement bool
}

// Record is what the engine remembers of one authorized sign, and what Restore rebuilds from the
// audit log. Amounts are the per-nonce window contributions (0 for sweeps and cancels).
type Record struct {
	At     time.Time
	Nonce  uint64
	Class  string
	Asset  string
	Amount *big.Int
	FeeWei *big.Int
	To     common.Address
	Value  *big.Int
	Data   []byte
	Gas    uint64
	MaxFee *big.Int
	Tip    *big.Int
}

type nonceState struct {
	first   time.Time
	last    Record
	counted map[string]*big.Int // window key -> max amount ever authorized for this nonce
}

// Engine evaluates intents against a Policy and keeps the Policy Window state.
type Engine struct {
	mu     sync.Mutex
	p      *Policy
	now    func() time.Time
	nonces map[uint64]*nonceState
	rate   []time.Time
}

// New returns an empty engine; call Restore for each historical authorized record, in order.
func New(p *Policy, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{p: p, now: now, nonces: map[uint64]*nonceState{}}
}

// Policy returns the enforced policy.
func (e *Engine) Policy() *Policy { return e.p }

// Authorize evaluates in and, under the engine lock, calls persist(decision) with the verdict
// (the audit append). Only if persist returns nil is an allowed decision committed to the window
// state; if it fails, nothing is committed and the error is returned (no signature may follow).
// Holding the lock across persist makes evaluate-record atomic across concurrent requests.
func (e *Engine) Authorize(in *intent.Intent, persist func(Decision) error) (Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.prune(now)
	d := e.evaluate(in, now)
	if persist != nil {
		if err := persist(d); err != nil {
			return d, err
		}
	}
	if d.Allowed {
		e.commit(recordOf(in, d, now))
	}
	return d, nil
}

// Evaluate is Authorize without persistence or commit (tests, dry runs).
func (e *Engine) Evaluate(in *intent.Intent) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.prune(now)
	return e.evaluate(in, now)
}

// Restore replays one authorized record (from the audit log) into the window state.
func (e *Engine) Restore(r Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commit(r)
	e.prune(e.now())
}

func recordOf(in *intent.Intent, d Decision, at time.Time) Record {
	return Record{At: at, Nonce: in.Nonce, Class: d.Class, Asset: d.Asset, Amount: d.Amount, FeeWei: d.FeeWei,
		To: in.To, Value: in.Value, Data: in.Data, Gas: in.Gas, MaxFee: in.MaxFee, Tip: in.Tip}
}

func (e *Engine) prune(now time.Time) {
	for n, s := range e.nonces {
		if now.Sub(s.first) > retention {
			delete(e.nonces, n)
		}
	}
	cut := 0
	for cut < len(e.rate) && now.Sub(e.rate[cut]) >= time.Minute {
		cut++
	}
	e.rate = e.rate[cut:]
}

// contributions is what a payout adds to each window key: the moved amount in its asset, and the
// worst-case fee (plus the value, for native) in the native window.
func contributions(class, asset string, amount, fee *big.Int) map[string]*big.Int {
	if class != intent.OpPayout {
		return nil
	}
	m := map[string]*big.Int{}
	if asset == NativeKey {
		m[NativeKey] = new(big.Int).Add(amount, fee)
		return m
	}
	m[asset] = new(big.Int).Set(amount)
	m[NativeKey] = new(big.Int).Set(fee)
	return m
}

func (e *Engine) commit(r Record) {
	s, ok := e.nonces[r.Nonce]
	if !ok {
		s = &nonceState{first: r.At, counted: map[string]*big.Int{}}
		e.nonces[r.Nonce] = s
	}
	s.last = r
	for k, v := range contributions(r.Class, r.Asset, orZero(r.Amount), orZero(r.FeeWei)) {
		if cur, ok := s.counted[k]; !ok || v.Cmp(cur) > 0 {
			s.counted[k] = v
		}
	}
	e.rate = append(e.rate, r.At)
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

func refuse(d Decision, rule string) Decision { d.Allowed, d.Rule = false, rule; return d }

func (e *Engine) evaluate(in *intent.Intent, now time.Time) Decision {
	p := e.p
	d := Decision{Class: in.Operation, Amount: new(big.Int), FeeWei: new(big.Int).Mul(new(big.Int).SetUint64(in.Gas), in.MaxFee)}
	if !p.Enabled {
		return refuse(d, RuleKillSwitch)
	}
	if len(e.rate) >= p.RatePerMinute {
		return refuse(d, RuleRateLimit)
	}
	if in.ChainID != p.ChainID {
		return refuse(d, RuleChainPin)
	}
	if in.Nonce == intent.MaxNonce {
		return refuse(d, RuleReservedNonce)
	}
	switch in.Operation {
	case intent.OpPayout, intent.OpTreasurySweep, intent.OpCancel:
	default:
		return refuse(d, RuleUnknownOperation)
	}

	// ---- data shape, asset, recipient
	var asset Asset
	d.Recipient = in.To
	switch {
	case len(in.Data) == 0:
		asset = p.Native
		d.Amount = new(big.Int).Set(in.Value)
	default:
		if len(in.Data) < 4 {
			return refuse(d, RuleDataMalformed)
		}
		d.Selector = hex.EncodeToString(in.Data[:4])
		switch {
		case bytes.Equal(in.Data[:4], selApprove):
			return refuse(d, RuleApproveRefused)
		case !bytes.Equal(in.Data[:4], selTransfer):
			return refuse(d, RuleSelectorRefused)
		}
		if len(in.Data) != 68 || !bytes.Equal(in.Data[4:16], make([]byte, 12)) {
			return refuse(d, RuleDataMalformed)
		}
		tok, ok := p.Tokens[in.To]
		if !ok {
			return refuse(d, RuleAssetNotAllowed)
		}
		if in.Value.Sign() != 0 {
			return refuse(d, RuleERC20ValueNonZero)
		}
		asset = tok
		d.Recipient = common.BytesToAddress(in.Data[16:36])
		d.Amount = new(big.Int).SetBytes(in.Data[36:68])
	}
	d.Asset = asset.Key

	// ---- class vs recipient
	switch in.Operation {
	case intent.OpCancel:
		if in.Value.Sign() != 0 || len(in.Data) != 0 || in.To != p.Hot {
			return refuse(d, RuleCancelShape)
		}
	case intent.OpTreasurySweep:
		if !p.HasWarm {
			return refuse(d, RuleTreasuryDisabled)
		}
		if d.Recipient != p.Warm {
			return refuse(d, RuleClassMismatch)
		}
		if _, isTok := p.Tokens[d.Recipient]; isTok {
			return refuse(d, RuleRecipientToken)
		}
	case intent.OpPayout:
		switch {
		case d.Recipient == (common.Address{}):
			return refuse(d, RuleRecipientZero)
		case d.Recipient == p.Hot:
			return refuse(d, RuleRecipientHot)
		case p.HasWarm && d.Recipient == p.Warm:
			return refuse(d, RuleClassMismatch) // moving funds to Warm is a Treasury Sweep, not a payout
		}
		if _, isTok := p.Tokens[d.Recipient]; isTok {
			return refuse(d, RuleRecipientToken)
		}
		if _, denied := p.Denied[d.Recipient]; denied {
			return refuse(d, RuleRecipientDenied)
		}
	}
	// A Cancel/sweep native transfer aimed at a token contract is covered above (token check on Recipient).

	// ---- gas and fees
	gasCap := asset.GasCap
	if in.Operation == intent.OpCancel {
		gasCap = p.Native.GasCap
	}
	if in.Gas > gasCap {
		return refuse(d, RuleGasCap)
	}
	if in.MaxFee.Cmp(p.FeeCeiling) > 0 {
		return refuse(d, RuleFeeCeiling)
	}
	if in.Tip.Cmp(in.MaxFee) > 0 {
		return refuse(d, RuleTipAboveFee)
	}

	// ---- replacement of a nonce we already signed
	prior, seen := e.nonces[in.Nonce]
	if seen {
		d.Replacement = true
		if in.Operation != intent.OpCancel && !isFeeBump(prior.last, in) {
			return refuse(d, RuleReplacementNotBump)
		}
	}

	// ---- caps
	switch in.Operation {
	case intent.OpTreasurySweep:
		if d.Amount.Cmp(asset.TreasuryMaxPerTx) > 0 {
			return refuse(d, RuleTreasuryMaxPerTx)
		}
	case intent.OpPayout:
		if d.Amount.Cmp(asset.MaxPerTx) > 0 {
			return refuse(d, RuleMaxPerTx)
		}
		for key, add := range contributions(in.Operation, d.Asset, d.Amount, d.FeeWei) {
			a := p.Native
			if key != NativeKey {
				a = p.Tokens[common.HexToAddress(key)]
			}
			if rule := e.windowRule(a, key, in.Nonce, prior, add, now); rule != "" {
				return refuse(d, rule)
			}
		}
	}

	d.Allowed = true
	switch {
	case in.Operation == intent.OpCancel:
		d.Rule = AcceptCancel
	case d.Replacement:
		d.Rule = AcceptFeeBump
	case in.Operation == intent.OpTreasurySweep:
		d.Rule = AcceptTreasurySweep
	default:
		d.Rule = AcceptPayout
	}
	return d
}

// windowRule checks the 1 h and 24 h caps for one window key. Each nonce counts once, at the
// maximum ever authorized for it (only one version of a nonce can be mined), dated from its first
// authorization.
func (e *Engine) windowRule(a Asset, key string, nonce uint64, prior *nonceState, add *big.Int, now time.Time) string {
	for _, w := range []struct {
		d    time.Duration
		cap  *big.Int
		rule string
	}{{window1h, a.Window1h, RuleWindow1h}, {window24h, a.Window24h, RuleWindow24h}} {
		sum := new(big.Int)
		for n, s := range e.nonces {
			if n == nonce || now.Sub(s.first) >= w.d {
				continue
			}
			if v, ok := s.counted[key]; ok {
				sum.Add(sum, v)
			}
		}
		this := new(big.Int).Set(add)
		first := now
		if prior != nil {
			first = prior.first
			if v, ok := prior.counted[key]; ok && v.Cmp(this) > 0 {
				this = v
			}
		}
		if now.Sub(first) < w.d {
			sum.Add(sum, this)
		}
		if sum.Cmp(w.cap) > 0 {
			return w.rule
		}
	}
	return ""
}

// isFeeBump: same body as the last version signed for the nonce, neither fee field lower.
// Equal fees are accepted: that is a retry of a lost response, not a new transaction.
func isFeeBump(last Record, in *intent.Intent) bool {
	return last.To == in.To && last.Value.Cmp(in.Value) == 0 && bytes.Equal(last.Data, in.Data) &&
		last.Gas == in.Gas && in.MaxFee.Cmp(last.MaxFee) >= 0 && in.Tip.Cmp(last.Tip) >= 0
}

// Lower is a helper for callers that need the canonical lower-case 0x form.
func Lower(a common.Address) string { return strings.ToLower(a.Hex()) }
