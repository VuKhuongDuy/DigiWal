package audit

import (
	"context"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/ledger"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

// LedgerIntegrity runs L1-L9 on every run (docs/design/reconciliation-audit.md "Ledger integrity"). The rules
// are Immediate, so a finding is recorded the first time it is seen. A finding whose cause is gone is
// resolved: every reference that has an open finding is re-evaluated against this run's result.
type LedgerIntegrity struct {
	Src   *source.LedgerChecks
	Store *store.Store
	Every time.Duration // default 5m
	Now   func() time.Time
}

func (a *LedgerIntegrity) Name() string { return "ledger_integrity" }

func (a *LedgerIntegrity) Interval() time.Duration {
	if a.Every > 0 {
		return a.Every
	}
	return 5 * time.Minute
}

func (a *LedgerIntegrity) Run(ctx context.Context) (job.Outcome, error) {
	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now().UTC()
	}
	found, err := a.Src.Check(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	byRef := map[string][]finding.Violation{}
	for _, f := range found {
		byRef[f.Ref] = append(byRef[f.Ref], f.V)
	}
	// A rule that hit its cap reported only some offenders, so absence from the result proves nothing:
	// resolve nothing this run.
	perRule := map[string]int{}
	truncated := false
	for _, f := range found {
		perRule[f.V.Rule]++
		truncated = truncated || perRule[f.V.Rule] >= source.MaxPerRule
	}
	if !truncated {
		open, err := a.Store.OpenRefs(ctx, finding.FlowLedger)
		if err != nil {
			return job.Outcome{}, err
		}
		for _, ref := range open {
			if _, ok := byRef[ref]; !ok {
				byRef[ref] = nil // nothing found for it now: resolve
			}
		}
	}
	runID := job.RunID(ctx)
	for ref, vs := range byRef {
		if err := a.Store.Reconcile(ctx, runID, finding.FlowLedger, ref, ledger.Rules, vs, now, 0); err != nil {
			return job.Outcome{}, err
		}
	}
	warn, crit, err := a.Store.CountOpen(ctx, finding.FlowLedger)
	return job.Outcome{Warn: warn, Critical: crit}, err
}
