// Package treasury holds rules T1-T5 and the classification of Treasury Movements
// (docs/design/reconciliation-audit.md, "Treasury and Hot Wallet signing"). Pure, like the other rule sets.
package treasury

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"digiwal/services/auditor/internal/finding"
)

// Rules is every treasury rule; one that is not violated resolves its open finding.
var Rules = []string{"T1", "T2", "T3", "T4", "T5"}

const (
	Hot      = "hot"
	Warm     = "warm"
	Cold     = "cold"
	Deposit  = "deposit"
	External = "external"
)

const (
	CNWithdrawalService = "withdrawal-service"
	CNSweeper           = "sweeper"
)

// Addresses are the tier addresses, lower case. Cold may be empty (no Cold address configured yet).
type Addresses struct{ Hot, Warm, Cold string }

// TierOf names the tier of addr. isDeposit says whether it is one of our users' deposit addresses.
func (a Addresses) TierOf(addr string, isDeposit func(string) bool) string {
	addr = strings.ToLower(addr)
	switch {
	case addr == "":
		return External
	case addr == a.Hot:
		return Hot
	case addr == a.Warm:
		return Warm
	case a.Cold != "" && addr == a.Cold:
		return Cold
	case isDeposit != nil && isDeposit(addr):
		return Deposit
	}
	return External
}

func isTier(t string) bool { return t == Hot || t == Warm || t == Cold }

// Classify gives the Treasury Movement kind for a transfer between tiers.
func Classify(from, to string) string {
	switch {
	case isTier(from) && isTier(to):
		return from + "_to_" + to
	case from == Deposit && to == Hot:
		return "sweep"
	case from == Hot && to == Deposit:
		return "gas_topup"
	case from == Hot:
		return "withdrawal"
	case isTier(to):
		return "deposit_into_tier"
	}
	return "unexplained"
}

// Movement is a transfer with a tier on at least one side.
type Movement struct {
	ChainID  int64
	TxHash   string
	LogIndex int64 // -1 native
	Token    string
	AssetID  string
	Amount   *big.Int
	From, To string
	FromTier string
	ToTier   string
	Kind     string
	Block    int64
	Time     time.Time
}

// Ref is the movement's reference in findings.
func (m Movement) Ref() string { return fmt.Sprintf("mv:%d:%s:%d", m.ChainID, m.TxHash, m.LogIndex) }

func imm(rule, sev string, d map[string]any) *finding.Violation {
	return &finding.Violation{Rule: rule, Severity: sev, Immediate: true, Detail: d}
}

// NonceRef is the reference of T1.
func NonceRef(chainID int64) string { return fmt.Sprintf("nonce:%d", chainID) }

// TxRef is the reference of a transaction sent by the Hot Wallet (T2, T4).
func TxRef(chainID int64, hash string) string {
	return fmt.Sprintf("tx:%d:%s", chainID, strings.ToLower(hash))
}

// SigRef is the reference of a Hot Wallet signature (T3).
func SigRef(key string) string { return "sig:" + key }

// T1: the chain has not seen more Hot Wallet nonces than wallet-service ever handed out; more means
// something signed with the Hot key outside wallet-service. chainCount must be read BEFORE nextNonce:
// next_nonce only grows, so that order cannot produce a false alarm.
func T1(chainCount, nextNonce uint64) *finding.Violation {
	if chainCount <= nextNonce {
		return nil
	}
	return imm("T1", finding.Critical, map[string]any{"problem": "the Hot address has sent more transactions than wallet-service allocated nonces for",
		"chain.transaction_count": chainCount, "wallet.next_nonce": nextNonce})
}

// HotTx is a mined transaction sent by the Hot Wallet.
type HotTx struct {
	Hash        string
	Nonce       uint64
	HasMovement bool // it moved value out of Hot
}

// Known is what wallet-service stored: every tx hash it ever signed for the Hot Wallet (the original, each
// Fee Bump and cancel), and the nonces those were signed for.
type Known struct {
	Hashes map[string]bool
	Nonces map[uint64]bool
}

// T2/T4: a mined Hot Wallet tx must be one wallet-service signed. If its nonce was handed out but this is
// another tx, it replaced ours (T2). If no signature exists for the nonce at all, it was never ours: T4
// when it moved value, T2 otherwise. Returns nil when it is a stored version.
func CheckHotTx(h HotTx, k Known) *finding.Violation {
	if k.Hashes[strings.ToLower(h.Hash)] {
		return nil
	}
	d := map[string]any{"tx_hash": h.Hash, "nonce": h.Nonce}
	switch {
	case k.Nonces[h.Nonce]:
		d["problem"] = "a mined Hot Wallet tx is not any version wallet-service signed for its nonce"
		return imm("T2", finding.Critical, d)
	case h.HasMovement:
		d["problem"] = "value left the Hot Wallet in a transaction wallet-service never signed"
		return imm("T4", finding.Critical, d)
	}
	d["problem"] = "a mined Hot Wallet tx has no signature at all"
	return imm("T2", finding.Critical, d)
}

// Signature is a row of hot_wallet_signatures.
type Signature struct {
	Key, CallerCN    string
	WithdrawalExists bool // for a "withdrawal:<id>" key: withdrawal-service has that request
}

// SignerPolicy: key prefix -> callers allowed to use it. Mirrors wallet-service's authz matrix; a prefix
// or caller missing here is a finding, which is the point. "treasury:" is not built yet and may be
// signed by either service that will own Treasury Sweeps.
var SignerPolicy = map[string][]string{
	"withdrawal:": {CNWithdrawalService},
	"gas-topup:":  {CNSweeper},
	"sweep:":      {CNSweeper},
	"treasury:":   {CNSweeper, CNWithdrawalService},
}

// T3: every Hot signature has a known purpose and an allowed caller; a withdrawal key must belong to a request.
func T3(s Signature) *finding.Violation {
	for prefix, callers := range SignerPolicy {
		if !strings.HasPrefix(s.Key, prefix) {
			continue
		}
		allowed := false
		for _, c := range callers {
			allowed = allowed || c == s.CallerCN
		}
		d := map[string]any{"key": s.Key, "caller_cn": s.CallerCN}
		switch {
		case !allowed:
			d["problem"] = "the signature was requested by a caller not allowed to use this kind of key"
			return imm("T3", finding.Critical, d)
		case prefix == "withdrawal:" && !s.WithdrawalExists:
			d["problem"] = "a withdrawal key with no withdrawal request behind it"
			return imm("T3", finding.Critical, d)
		}
		return nil
	}
	return imm("T3", finding.Critical, map[string]any{"problem": "a Hot Wallet signature with an unknown purpose", "key": s.Key, "caller_cn": s.CallerCN})
}

// T5: Warm and Cold are moved by the offline CLI and no record exists on our side, so a movement out of
// them is informational. One that leaves our tiers altogether is worth a look (warn).
func T5(m Movement) *finding.Violation {
	if m.FromTier != Warm && m.FromTier != Cold {
		return nil
	}
	d := map[string]any{"from": m.From, "to": m.To, "amount": m.Amount.String(), "kind": m.Kind, "block": m.Block}
	if isTier(m.ToTier) {
		d["problem"] = "movement out of " + m.FromTier + ", no operator-side record exists (offline CLI)"
		return imm("T5", finding.Info, d)
	}
	d["problem"] = "movement out of " + m.FromTier + " to an address outside our tiers"
	return imm("T5", finding.Warn, d)
}
