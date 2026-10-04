// Package ledger names rules L1-L9 (docs/design/reconciliation-audit.md, "Ledger integrity"). Unlike W and
// D they are not a function of one transaction's facts: each is an aggregate over the whole ledger
// database, evaluated in one REPEATABLE READ, READ ONLY snapshot (internal/source/ledger.go). They
// are Immediate: a snapshot cannot disagree with itself, so there is no lag to wait out.
package ledger

import "digiwal/services/auditor/internal/finding"

// Rules is every ledger rule; one that is not violated resolves its open findings.
var Rules = []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9"}

// Found is a violation and the thing it is about ("tx:<id>", "account:<id>", "asset:<id>", "lock:<key>",
// "entry:<id>", "gas:<key>", "trigger:entries").
type Found struct {
	Ref string
	V   finding.Violation
}

// New builds an Immediate critical violation.
func New(rule, ref string, detail map[string]any) Found {
	return Found{Ref: ref, V: finding.Violation{Rule: rule, Severity: finding.Critical, Immediate: true, Detail: detail}}
}

// TriggerNames are the immutability triggers on entries (ledger-service migration 000001) that L9 requires.
var TriggerNames = []string{"entries_no_update_delete", "entries_no_truncate"}
