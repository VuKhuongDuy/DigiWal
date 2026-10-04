// Package withdrawal holds rules W1-W10 (docs/design/reconciliation-audit.md, Decision (a)). They are pure:
// Facts in, Violations out. Reading the stores is internal/source's job, deciding what to record is the
// store's, so every rule is testable with a struct literal.
package withdrawal

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"digiwal/services/auditor/internal/finding"
)

// Rules is every rule evaluated for a withdrawal. A rule that is not violated resolves an open finding
// for it, so the store needs the full list, not only the violated ones.
var Rules = []string{"W1", "W2", "W3", "W4", "W5", "W6", "W7", "W8", "W9", "W10"}

const (
	StatusConfirmed = "confirmed"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"

	ReasonTxReverted = "tx_reverted"

	PurposeWithdrawal         = "withdrawal"
	PurposeWithdrawalReverted = "withdrawal_reverted"

	SignerCN = "withdrawal-service"

	// selector of ERC-20 transfer(address,uint256)
	transferSelector = "a9059cbb"
)

// IsFinal reports whether a status is one the auditor looks at (invariant 3).
func IsFinal(status string) bool {
	return status == StatusConfirmed || status == StatusFailed || status == StatusRejected
}

// Request is the withdrawal-service row.
type Request struct {
	ID               string
	UserID           string
	AssetID          string
	Amount           *big.Int
	ToAddress        string
	Status           string
	RejectReason     string
	TxHash           string
	Nonce            *int64
	ChainID          *int64
	ConfirmedBlock   *int64
	SettleLedgerTxID string
	GasPrice         *big.Int // effective gas price recorded at confirmation; 0 posts no Gas Fee
	UpdatedAt        time.Time
	VersionTxHashes  []string // withdrawal_txs: the original and every Fee Bump
}

type Lock struct {
	Status  string // active | settled | released
	Amount  *big.Int
	AssetID string
	UserID  string
}

type Entry struct {
	Amount  *big.Int
	Kind    string // account kind: user | chain | gas_expense
	AssetID string
}

type Settle struct {
	ID      string
	Entries []Entry
}

type Signature struct {
	Nonce    int64
	CallerCN string
}

type History struct {
	Status  string
	TxHash  string
	Amount  string
	AssetID string
}

type Asset struct {
	Type            string // native | erc20
	ContractAddress string
}

// ChainTx is what the chain says about one tx hash. Found is false if the node does not know it.
type ChainTx struct {
	Found   bool
	Mined   bool
	Success bool
	Block   int64
	To      string
	Value   *big.Int
	Input   []byte
}

// Facts is everything the rules need about one withdrawal, read from every store that should know it.
type Facts struct {
	Req           *Request // nil: no such request (an orphan Settle)
	Lock          *Lock
	Settle        *Settle
	Refunds       int    // distinct correction transactions that reverse the Settle's entries
	GasFeePurpose string // purpose of gas:{chain}:{tx_hash}; empty if none posted
	Sig           *Signature
	History       *History
	Asset         *Asset
	Chain         map[string]ChainTx // lower-case tx hash -> chain state, for every version
	SharedBy      []string           // other requests that claim one of this request's tx hashes
}

// Config holds the tunables (Objectives table).
type Config struct {
	// Escalate is how long a warn finding for W5 stays a warn before it is critical.
	Escalate time.Duration
}

// Evaluate applies W1..W10 to f.
func Evaluate(f Facts) []finding.Violation {
	var out []finding.Violation
	add := func(v *finding.Violation) {
		if v != nil {
			out = append(out, *v)
		}
	}
	if f.Req == nil {
		add(w2Orphan(f))
		return out
	}
	add(w1(f))
	add(w2(f))
	add(w3(f))
	add(w4(f))
	add(w5(f))
	add(w6(f))
	add(w7(f))
	add(w8(f))
	add(w9(f))
	add(w10(f))
	return out
}

func crit(rule string, detail map[string]any) *finding.Violation {
	return &finding.Violation{Rule: rule, Severity: finding.Critical, Detail: detail}
}

func lowerEq(a, b string) bool { return strings.EqualFold(a, b) }

func str(b *big.Int) string {
	if b == nil {
		return ""
	}
	return b.String()
}

// W1: a confirmed request has exactly one withdrawal_settle, and settle_ledger_tx_id is it.
func w1(f Facts) *finding.Violation {
	r := f.Req
	if r.Status != StatusConfirmed {
		return nil
	}
	if f.Settle == nil {
		return crit("W1", map[string]any{"problem": "confirmed request has no withdrawal_settle in the ledger", "settle_ledger_tx_id": r.SettleLedgerTxID})
	}
	if r.SettleLedgerTxID != f.Settle.ID {
		return crit("W1", map[string]any{"problem": "settle_ledger_tx_id does not point at the ledger's settle",
			"request.settle_ledger_tx_id": r.SettleLedgerTxID, "ledger.settle_id": f.Settle.ID})
	}
	return nil
}

// W2: a withdrawal_settle exists only for a request that is confirmed, or failed after a refund. An orphan
// Settle (no request at all) is reported by w2Orphan.
func w2(f Facts) *finding.Violation {
	r := f.Req
	if f.Settle == nil {
		return nil
	}
	switch {
	case r.Status == StatusConfirmed && f.Refunds > 0:
		return crit("W2", map[string]any{"problem": "request is confirmed but its Settle was refunded", "refunds": f.Refunds})
	case r.Status != StatusConfirmed && r.Status != StatusFailed:
		return crit("W2", map[string]any{"problem": "a Settle exists for a request that is not confirmed", "request.status": r.Status, "ledger.settle_id": f.Settle.ID})
	}
	return nil
}

func w2Orphan(f Facts) *finding.Violation {
	if f.Settle == nil {
		return nil
	}
	return crit("W2", map[string]any{"problem": "a withdrawal_settle exists for a request withdrawal-service has no record of", "ledger.settle_id": f.Settle.ID})
}

// W3: amount and asset agree across request, Lock, Settle's user debit and the history document.
func w3(f Facts) *finding.Violation {
	r := f.Req
	type side struct {
		name, amount, asset string
	}
	sides := []side{{"request", str(r.Amount), r.AssetID}}
	if f.Lock != nil {
		sides = append(sides, side{"lock", str(f.Lock.Amount), f.Lock.AssetID})
	}
	if f.Settle != nil {
		for _, e := range f.Settle.Entries {
			if e.Kind == "user" {
				sides = append(sides, side{"settle_debit", str(new(big.Int).Neg(e.Amount)), e.AssetID})
			}
		}
	}
	if f.History != nil {
		sides = append(sides, side{"history", f.History.Amount, f.History.AssetID})
	}
	if r.Status == StatusConfirmed && f.Lock == nil {
		return crit("W3", map[string]any{"problem": "confirmed request has no Lock in the ledger"})
	}
	detail := map[string]any{}
	bad := false
	for _, s := range sides {
		detail[s.name] = map[string]string{"amount": s.amount, "asset": s.asset}
		if s.amount != sides[0].amount || !lowerEq(s.asset, sides[0].asset) {
			bad = true
		}
	}
	if bad {
		detail["problem"] = "amount or asset differs between stores"
		return crit("W3", detail)
	}
	return nil
}

// W4: a failed or rejected request has no active Lock and no Settle that was not refunded exactly once.
func w4(f Facts) *finding.Violation {
	r := f.Req
	if r.Status != StatusFailed && r.Status != StatusRejected {
		return nil
	}
	if f.Lock != nil && f.Lock.Status == "active" {
		return crit("W4", map[string]any{"problem": "request is " + r.Status + " but its Lock is still active", "lock.amount": str(f.Lock.Amount)})
	}
	if r.Status == StatusRejected && f.Lock != nil {
		return crit("W4", map[string]any{"problem": "rejected request has a Lock; nothing should have been reserved", "lock.status": f.Lock.Status})
	}
	if f.Settle != nil && f.Refunds != 1 {
		return crit("W4", map[string]any{"problem": "failed request was Settled and not refunded by exactly one correction", "refunds": f.Refunds, "ledger.settle_id": f.Settle.ID})
	}
	return nil
}

// W5: status and tx_hash in the history document match the request. Lag is expected, so this is a warn
// that becomes critical if it stays.
func w5(f Facts) *finding.Violation {
	r := f.Req
	var want string
	switch r.Status {
	case StatusConfirmed:
		want = "confirmed"
	case StatusFailed:
		want = "failed"
	default:
		return nil // a rejected request never reaches history
	}
	mk := func(detail map[string]any) *finding.Violation {
		return &finding.Violation{Rule: "W5", Severity: finding.Warn, EscalateTo: finding.Critical, Detail: detail}
	}
	if f.History == nil {
		return mk(map[string]any{"problem": "no history document", "request.status": r.Status})
	}
	if f.History.Status != want {
		return mk(map[string]any{"problem": "history status differs", "request.status": r.Status, "history.status": f.History.Status})
	}
	if r.TxHash != "" && !lowerEq(f.History.TxHash, r.TxHash) {
		return mk(map[string]any{"problem": "history tx_hash differs", "request.tx_hash": r.TxHash, "history.tx_hash": f.History.TxHash})
	}
	return nil
}

// W6: a request that was given a nonce has exactly one signature, made for withdrawal-service, with that
// nonce. Immediate: the signature table is written before broadcast, long before the request is final.
func w6(f Facts) *finding.Violation {
	r := f.Req
	imm := func(d map[string]any) *finding.Violation {
		return &finding.Violation{Rule: "W6", Severity: finding.Critical, Immediate: true, Detail: d}
	}
	if r.Nonce == nil {
		if r.Status == StatusConfirmed {
			return imm(map[string]any{"problem": "confirmed request has no nonce"})
		}
		return nil
	}
	switch {
	case f.Sig == nil:
		return imm(map[string]any{"problem": "request has a nonce but wallet-service has no signature for its key", "request.nonce": *r.Nonce})
	case f.Sig.CallerCN != SignerCN:
		return imm(map[string]any{"problem": "signature was requested by another caller", "signature.caller_cn": f.Sig.CallerCN})
	case f.Sig.Nonce != *r.Nonce:
		return imm(map[string]any{"problem": "signature nonce differs from the request's", "request.nonce": *r.Nonce, "signature.nonce": f.Sig.Nonce})
	}
	return nil
}

// W7: the mined tx of a confirmed request did what the request says.
func w7(f Facts) *finding.Violation {
	r := f.Req
	if r.Status != StatusConfirmed {
		return nil
	}
	if r.TxHash == "" {
		return crit("W7", map[string]any{"problem": "confirmed request has no tx_hash"})
	}
	c := f.Chain[strings.ToLower(r.TxHash)]
	d := map[string]any{"tx_hash": r.TxHash}
	switch {
	case !c.Found || !c.Mined:
		d["problem"] = "the chain does not know a mined tx with this hash"
		return crit("W7", d)
	case !c.Success:
		d["problem"] = "the tx reverted"
		return crit("W7", d)
	case r.ConfirmedBlock == nil || *r.ConfirmedBlock != c.Block:
		d["problem"] = "block differs"
		d["request.confirmed_block"], d["chain.block"] = r.ConfirmedBlock, c.Block
		return crit("W7", d)
	case f.Asset == nil:
		d["problem"] = "the asset is not in the ledger's registry, cannot verify the payload"
		return crit("W7", d)
	}
	to, amount, ok := payload(f.Asset, c)
	if !ok || !lowerEq(to, r.ToAddress) || amount == nil || amount.Cmp(r.Amount) != 0 {
		d["problem"] = "recipient or amount on chain differs from the request"
		d["request"] = map[string]string{"to": r.ToAddress, "amount": str(r.Amount)}
		d["chain"] = map[string]string{"to": to, "amount": str(amount)}
		return crit("W7", d)
	}
	return nil
}

// payload extracts (recipient, amount) from the tx: a plain value transfer for the native asset, a
// transfer(to, amount) call to the token contract for an ERC-20.
func payload(a *Asset, c ChainTx) (to string, amount *big.Int, ok bool) {
	if a.Type == "native" {
		return c.To, c.Value, true
	}
	if !lowerEq(c.To, a.ContractAddress) || len(c.Input) != 4+32+32 || fmt.Sprintf("%x", c.Input[:4]) != transferSelector {
		return c.To, nil, false
	}
	return "0x" + fmt.Sprintf("%x", c.Input[4+12:4+32]), new(big.Int).SetBytes(c.Input[4+32:]), true
}

// W8: no two requests share a mined tx hash, and no request has two mined versions. Immediate.
func w8(f Facts) *finding.Violation {
	r := f.Req
	if len(f.SharedBy) > 0 {
		return &finding.Violation{Rule: "W8", Severity: finding.Critical, Immediate: true,
			Detail: map[string]any{"problem": "another request claims the same tx hash", "other_requests": f.SharedBy}}
	}
	var mined []string
	for h, c := range f.Chain {
		if c.Found && c.Mined {
			mined = append(mined, h)
		}
	}
	if len(mined) > 1 {
		return &finding.Violation{Rule: "W8", Severity: finding.Critical, Immediate: true,
			Detail: map[string]any{"problem": "more than one version of the tx was mined", "tx_hashes": mined, "request.id": r.ID}}
	}
	return nil
}

// W9: a confirmed request has its Gas Fee posted (warn: ledger.md #6 accepts a lost one).
func w9(f Facts) *finding.Violation {
	r := f.Req
	if r.Status != StatusConfirmed || (r.GasPrice != nil && r.GasPrice.Sign() == 0) {
		return nil
	}
	warn := func(p string) *finding.Violation {
		return &finding.Violation{Rule: "W9", Severity: finding.Warn, Detail: map[string]any{"problem": p, "tx_hash": r.TxHash}}
	}
	switch f.GasFeePurpose {
	case PurposeWithdrawal:
		return nil
	case "":
		return warn("no Gas Fee transaction in the ledger")
	}
	return warn("Gas Fee posted with purpose " + f.GasFeePurpose + ", expected " + PurposeWithdrawal)
}

// W10: a reverted tx: Lock Released, request failed with tx_reverted, Gas Fee posted as
// withdrawal_reverted. Applies when the chain says it reverted or the request says it did.
func w10(f Facts) *finding.Violation {
	r := f.Req
	if r.Status != StatusFailed || r.TxHash == "" {
		return nil
	}
	c, known := f.Chain[strings.ToLower(r.TxHash)]
	chainReverted := known && c.Found && c.Mined && !c.Success
	claimed := r.RejectReason == ReasonTxReverted
	if !chainReverted && !claimed {
		return nil
	}
	d := map[string]any{"tx_hash": r.TxHash, "request.reject_reason": r.RejectReason}
	switch {
	case claimed && !chainReverted:
		d["problem"] = "request says tx_reverted but the chain does not show a reverted tx"
	case chainReverted && !claimed:
		d["problem"] = "the tx reverted on chain but the request did not fail with tx_reverted"
	case f.Lock == nil || f.Lock.Status != "released":
		d["problem"] = "the Lock of a reverted tx is not released"
	case (r.GasPrice == nil || r.GasPrice.Sign() != 0) && f.GasFeePurpose != PurposeWithdrawalReverted:
		d["problem"] = "no Gas Fee posted as withdrawal_reverted"
		d["ledger.gas_fee_purpose"] = f.GasFeePurpose
	default:
		return nil
	}
	return crit("W10", d)
}
