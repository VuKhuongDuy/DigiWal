// Package custody is the aggregate reconciliation (docs/design/reconciliation-audit.md, "Aggregate
// reconciliation"): does the chain hold what the ledger says it holds? Pure arithmetic over figures the
// source layer reads; per asset.
//
//	on_chain_custody = sum of balances at Hot, Warm, Cold and every deposit address, at block B
//	ledger_custody   = -ChainAccount.total_balance
//	explained_delta  = deposits observed up to B not yet credited          (lag, and held Flagged Deposits)
//	                 - withdrawals mined up to B not yet Settled
//	                 - gas paid up to B not yet posted
//	                 + operator capital                                    (see Inputs.Capital)
//	residual         = on_chain_custody - ledger_custody - explained_delta
package custody

import (
	"math/big"

	"digiwal/services/auditor/internal/finding"
)

// Rules: C1 the residual, C2 solvency, C3 Hot coverage of active Locks. (The design names C1; C2 and C3 are
// the two "also" checks it lists under the same heading.)
var Rules = []string{"C1", "C2", "C3"}

// Inputs are the figures for one asset. All amounts are minor units.
type Inputs struct {
	AssetID, Symbol string
	Block           int64

	OnChain    *big.Int // Hot + Warm + Cold + deposit addresses, at Block
	HotOnChain *big.Int // the Hot Wallet alone, at Block

	ChainAccount *big.Int // ledger: the Chain Account's total_balance (negative = custody held)
	UserTotal    *big.Int // ledger: sum of user Accounts' total_balance
	ActiveLocks  *big.Int // ledger: sum of active Locks

	ObservedUpToB      *big.Int // chain-watcher: transfers to deposit addresses mined at or before Block
	Credited           *big.Int // ledger: deposits credited to users
	FlaggedHeld        *big.Int // ledger: Flagged Deposits not approved (part of Observed - Credited; reported on its own)
	UnsettledWithdrawn *big.Int // withdrawals mined at or before Block that the ledger has not Settled
	UnpostedGas        *big.Int // native asset only: gas paid at or before Block that is not in the ledger yet

	// Capital is what the operator put into custody that no user owns: the Hot Wallet's gas float and
	// liquidity, funded from outside. The ledger never sees it (it is not a deposit), so it would show as a
	// permanent residual; it is configured (RECON_CAPITAL), not guessed. Zero by default.
	Capital   *big.Int
	Tolerance *big.Int // |residual| at or below this is a warn, above it critical
}

// Result is the reconciliation of one asset.
type Result struct {
	Inputs
	LedgerCustody *big.Int
	Explained     *big.Int
	Residual      *big.Int
}

func z(b *big.Int) *big.Int {
	if b == nil {
		return new(big.Int)
	}
	return b
}

// Compute applies the formula.
func Compute(in Inputs) Result {
	ledger := new(big.Int).Neg(z(in.ChainAccount))
	explained := new(big.Int).Sub(z(in.ObservedUpToB), z(in.Credited))
	explained.Sub(explained, z(in.UnsettledWithdrawn))
	explained.Sub(explained, z(in.UnpostedGas))
	explained.Add(explained, z(in.Capital))
	residual := new(big.Int).Sub(z(in.OnChain), ledger)
	residual.Sub(residual, explained)
	return Result{Inputs: in, LedgerCustody: ledger, Explained: explained, Residual: residual}
}

// Ref is the finding reference of an asset.
func Ref(assetID string) string { return "asset:" + assetID }

// Evaluate gives the findings for one asset. None is Immediate: the figures come from different stores read a
// moment apart, so a mismatch must be seen in two consecutive runs.
func Evaluate(r Result) []finding.Violation {
	var out []finding.Violation
	detail := func(problem string, extra map[string]any) map[string]any {
		d := map[string]any{"problem": problem, "symbol": r.Symbol, "block": r.Block,
			"on_chain": z(r.OnChain).String(), "ledger_custody": r.LedgerCustody.String(), "explained": r.Explained.String(), "residual": r.Residual.String()}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}

	// C1: the residual. Within the tolerance it is a warn that stays a warn (the accepted gas drift of
	// ledger.md #6 lives here); beyond it, critical.
	if r.Residual.Sign() != 0 {
		abs := new(big.Int).Abs(r.Residual)
		sev := finding.Critical
		if abs.Cmp(z(r.Tolerance)) <= 0 {
			sev = finding.Warn
		}
		out = append(out, finding.Violation{Rule: "C1", Severity: sev,
			Detail: detail("the chain holds a different amount than the ledger accounts for", map[string]any{"tolerance": z(r.Tolerance).String()})})
	}
	// C2: solvency. Users cannot be owed more than the chain holds. A withdrawal already mined but not yet
	// Settled has left the chain while the user's balance still includes it, so it is taken off the claim.
	owed := new(big.Int).Sub(z(r.UserTotal), z(r.UnsettledWithdrawn))
	if owed.Cmp(z(r.OnChain)) > 0 {
		out = append(out, finding.Violation{Rule: "C2", Severity: finding.Critical,
			Detail: detail("users are owed more than the chain holds", map[string]any{"user_balances": z(r.UserTotal).String(), "owed_after_in_flight_withdrawals": owed.String()})})
	}
	// C3: the Hot Wallet can pay out what is locked for withdrawal.
	if z(r.ActiveLocks).Cmp(z(r.HotOnChain)) > 0 {
		out = append(out, finding.Violation{Rule: "C3", Severity: finding.Warn,
			Detail: detail("the Hot Wallet holds less than the active withdrawal Locks", map[string]any{"hot": z(r.HotOnChain).String(), "active_locks": z(r.ActiveLocks).String()})})
	}
	return out
}
