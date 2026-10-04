// Package finding is the vocabulary shared by the rule sets and the store that records what they find
// (docs/design/reconciliation-audit.md, Decision (b)).
package finding

const (
	Info     = "info"
	Warn     = "warn"
	Critical = "critical"
)

const (
	FlowWithdrawal = "withdrawal"
	FlowDeposit    = "deposit"
	FlowTreasury   = "treasury"
	FlowLedger     = "ledger"
	FlowCustody    = "custody"
)

// Violation is one rule broken for one transaction.
type Violation struct {
	Rule     string
	Severity string
	// EscalateTo, if set, is the severity this becomes when it has stayed open longer than the
	// escalation window ("warn, then critical", W5 and D6).
	EscalateTo string
	// Immediate marks a check that cannot be a timing artefact between stores (invariant 4): it is
	// recorded the first time it is seen instead of the second.
	Immediate bool
	// Detail has both sides of the mismatch, store by store. No secrets.
	Detail map[string]any
}
