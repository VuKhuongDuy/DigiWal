// Package deposit holds rules D1-D7 (docs/design/reconciliation-audit.md, Decision (a)). Like the
// withdrawal rules they are pure: Facts in, Violations out.
package deposit

import (
	"math/big"
	"strings"

	"digiwal/services/auditor/internal/finding"
)

// Rules is every rule evaluated for a deposit; a rule that is not violated resolves its open finding.
var Rules = []string{"D1", "D2", "D3", "D4", "D5", "D6", "D7"}

const StatusConfirmed = "confirmed"

// Observed is chain-watcher's observed_transfers row.
type Observed struct {
	AssetID   string
	To        string
	UserID    string
	Amount    *big.Int
	Block     int64
	BlockHash string
	Status    string
}

// Ledger is the deposit Ledger Transaction: the user's credit and the account it went to.
type Ledger struct {
	ID      string
	UserID  string   // owner of the credited user Account
	AssetID string   // asset of that Account
	Amount  *big.Int // the user's Entry
}

type History struct {
	Status, Amount, AssetID string
}

// OnChain is what the chain says about the transfer (tx_hash, log_index).
type OnChain struct {
	Found     bool // the tx (native) or the Transfer log (ERC-20) exists
	Success   bool
	Block     int64
	BlockHash string // hash of the block the receipt names
	Canonical string // hash of the block currently at that height
	From      string // the sender of a native transfer; "" for an ERC-20 Transfer log
	To        string
	Amount    *big.Int
	Token     string // ERC-20 contract the Transfer log came from; "" for the native coin
	AssetID   string // resolved from Token via the ledger's asset registry; "" if unknown
}

// Facts is everything the rules need about one deposit, keyed by (chain_id, tx_hash, log_index).
type Facts struct {
	Observed *Observed
	Ledger   *Ledger
	Flagged  bool   // a flagged_deposits row exists
	Owner    string // wallet-service owner of the receiving address; "" if it is not a deposit address
	History  *History
	Chain    OnChain
	// Hot is wallet-service's Hot Wallet address, "" when unknown. A native transfer sent from it (a Gas Top-Up to a
	// deposit address) is Hot Wallet Outflow, which chain-watcher never counts as a deposit (deposit-flow.md #17).
	Hot string
}

func lowerEq(a, b string) bool { return strings.EqualFold(a, b) }

func str(b *big.Int) string {
	if b == nil {
		return ""
	}
	return b.String()
}

func crit(rule string, d map[string]any) *finding.Violation {
	return &finding.Violation{Rule: rule, Severity: finding.Critical, Detail: d}
}

// Evaluate applies D1..D7 to f.
func Evaluate(f Facts) []finding.Violation {
	var out []finding.Violation
	for _, v := range []*finding.Violation{d1(f), d2(f), d3(f), d4(f), d5(f), d6(f), d7(f)} {
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

func confirmed(f Facts) bool { return f.Observed != nil && f.Observed.Status == StatusConfirmed }

// D1: a confirmed observed transfer has exactly one deposit Ledger Transaction, or a Flagged Deposit
// (a Screening List sender), never both and never neither.
func d1(f Facts) *finding.Violation {
	if !confirmed(f) {
		return nil
	}
	switch {
	case f.Ledger != nil && f.Flagged:
		return crit("D1", map[string]any{"problem": "credited in the ledger and also flagged", "ledger.id": f.Ledger.ID})
	case f.Ledger == nil && !f.Flagged:
		return crit("D1", map[string]any{"problem": "confirmed transfer was neither credited nor flagged", "observed.amount": str(f.Observed.Amount)})
	}
	return nil
}

// D2: a deposit Ledger Transaction has a confirmed observed transfer behind it.
func d2(f Facts) *finding.Violation {
	if f.Ledger == nil {
		return nil
	}
	switch {
	case f.Observed == nil:
		return crit("D2", map[string]any{"problem": "credited in the ledger but chain-watcher never observed it", "ledger.id": f.Ledger.ID, "ledger.amount": str(f.Ledger.Amount)})
	case f.Observed.Status != StatusConfirmed:
		return crit("D2", map[string]any{"problem": "credited in the ledger but the observed transfer is not confirmed", "observed.status": f.Observed.Status, "ledger.id": f.Ledger.ID})
	}
	return nil
}

// D3: the credited user is the owner of the receiving address.
func d3(f Facts) *finding.Violation {
	if f.Observed == nil && f.Ledger == nil {
		return nil
	}
	d := map[string]any{"wallet.owner": f.Owner}
	bad := f.Owner == ""
	if f.Observed != nil {
		d["observed.user_id"] = f.Observed.UserID
		bad = bad || f.Observed.UserID != f.Owner
	}
	if f.Ledger != nil {
		d["ledger.user_id"] = f.Ledger.UserID
		bad = bad || f.Ledger.UserID != f.Owner
	}
	if !bad {
		return nil
	}
	d["problem"] = "credited user is not the owner of the receiving address"
	return crit("D3", d)
}

// D4: amount and asset agree across observed transfer, ledger Entry, history document and the chain.
func d4(f Facts) *finding.Violation {
	if f.Observed == nil && f.Ledger == nil {
		return nil
	}
	type side struct{ name, amount, asset string }
	var sides []side
	if f.Observed != nil {
		sides = append(sides, side{"observed", str(f.Observed.Amount), f.Observed.AssetID})
	}
	if f.Ledger != nil {
		sides = append(sides, side{"ledger", str(f.Ledger.Amount), f.Ledger.AssetID})
	}
	if f.History != nil {
		sides = append(sides, side{"history", f.History.Amount, f.History.AssetID})
	}
	if f.Chain.Found {
		sides = append(sides, side{"chain", str(f.Chain.Amount), f.Chain.AssetID})
	}
	d := map[string]any{}
	bad := false
	for _, s := range sides {
		d[s.name] = map[string]string{"amount": s.amount, "asset": s.asset}
		if s.amount != sides[0].amount || !lowerEq(s.asset, sides[0].asset) {
			bad = true
		}
	}
	if !bad {
		return nil
	}
	d["problem"] = "amount or asset differs between stores"
	return crit("D4", d)
}

// D5: the transfer exists in the canonical chain at the recorded block.
func d5(f Facts) *finding.Violation {
	if !confirmed(f) {
		return nil
	}
	o, c := f.Observed, f.Chain
	d := map[string]any{"observed.block": o.Block, "observed.block_hash": o.BlockHash}
	switch {
	case !c.Found:
		d["problem"] = "the chain has no such transfer"
	case !c.Success:
		d["problem"] = "the tx reverted, nothing was transferred"
	case c.Block != o.Block:
		d["problem"] = "mined in another block"
		d["chain.block"] = c.Block
	case !lowerEq(c.BlockHash, o.BlockHash) || !lowerEq(c.Canonical, o.BlockHash):
		d["problem"] = "the recorded block is no longer in the canonical chain (reorg)"
		d["chain.block_hash"], d["chain.canonical_hash"] = c.BlockHash, c.Canonical
	default:
		return nil
	}
	return crit("D5", d)
}

// D6: the history document says confirmed. History lags, so this is a warn that escalates.
func d6(f Facts) *finding.Violation {
	if !confirmed(f) {
		return nil
	}
	mk := func(p string, extra ...any) *finding.Violation {
		d := map[string]any{"problem": p}
		for i := 0; i+1 < len(extra); i += 2 {
			d[extra[i].(string)] = extra[i+1]
		}
		return &finding.Violation{Rule: "D6", Severity: finding.Warn, EscalateTo: finding.Critical, Detail: d}
	}
	if f.History == nil {
		return mk("no history document")
	}
	if f.History.Status != StatusConfirmed {
		return mk("history status differs", "history.status", f.History.Status)
	}
	return nil
}

// D7: a successful transfer to a deposit address that chain-watcher has no record of (missed by the watcher).
func d7(f Facts) *finding.Violation {
	if f.Observed != nil || !f.Chain.Found || !f.Chain.Success || f.Chain.Amount == nil || f.Chain.Amount.Sign() <= 0 || f.Owner == "" {
		return nil
	}
	if f.Hot != "" && f.Chain.Token == "" && lowerEq(f.Chain.From, f.Hot) {
		return nil
	}
	return crit("D7", map[string]any{"problem": "a transfer to a deposit address that chain-watcher never observed",
		"chain.to": f.Chain.To, "chain.amount": str(f.Chain.Amount), "chain.block": f.Chain.Block, "wallet.owner": f.Owner})
}
