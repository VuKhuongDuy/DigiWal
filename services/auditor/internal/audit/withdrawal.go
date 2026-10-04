// Package audit holds the jobs: they wire a rule set to the stores it reads and to the findings store.
package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/withdrawal"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

// WithdrawalAudit applies W1-W10 to every final withdrawal (docs/design/reconciliation-audit.md Decision (a)).
//
// Each run: (1) re-evaluates every reference that has a pending or open finding, so a mismatch is
// confirmed, resolved or escalated; (2) audits requests that became final since the cursor, once they
// have been final for the Grace Period (invariant 3); (3) looks for ledger Settles whose request is
// missing. A reference is evaluated at most once per run, which is what makes "seen in two consecutive
// runs" mean two runs (invariant 4).
type WithdrawalAudit struct {
	Src   *source.Withdrawals
	Store *store.Store

	Every    time.Duration // run interval (default 5m)
	Grace    time.Duration // how long a request is final before it is audited (default 2 runs)
	Escalate time.Duration // warn -> critical window for W5 (default 3 runs)
	Batch    int
	Now      func() time.Time
}

const (
	cursorRequests = "withdrawal_audit"
	cursorSettles  = "withdrawal_orphan_settles"
)

func (a *WithdrawalAudit) Name() string { return "withdrawal_audit" }

func (a *WithdrawalAudit) Interval() time.Duration {
	if a.Every > 0 {
		return a.Every
	}
	return 5 * time.Minute
}

func (a *WithdrawalAudit) grace() time.Duration {
	if a.Grace > 0 {
		return a.Grace
	}
	return 2 * a.Interval()
}

func (a *WithdrawalAudit) escalate() time.Duration {
	if a.Escalate > 0 {
		return a.Escalate
	}
	return 3 * a.Interval()
}

func (a *WithdrawalAudit) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *WithdrawalAudit) batch() int {
	if a.Batch > 0 {
		return a.Batch
	}
	return 200
}

func (a *WithdrawalAudit) Run(ctx context.Context) (job.Outcome, error) {
	runID := job.RunID(ctx)
	now := a.now()
	notAfter := now.Add(-a.grace())
	seen := map[string]bool{}

	eval := func(ref string) error {
		ref = strings.TrimPrefix(ref, "withdrawal:")
		if seen[ref] {
			return nil
		}
		seen[ref] = true
		f, err := a.Src.Gather(ctx, ref)
		if err != nil {
			return fmt.Errorf("reading %s: %w", ref, err)
		}
		return a.Store.Reconcile(ctx, runID, finding.FlowWithdrawal, ref, withdrawal.Rules, withdrawal.Evaluate(f), now, a.escalate())
	}

	open, err := a.Store.OpenRefs(ctx, finding.FlowWithdrawal)
	if err != nil {
		return job.Outcome{}, err
	}
	for _, ref := range open {
		if err := eval(ref); err != nil {
			return job.Outcome{}, err
		}
	}

	if err := a.scan(ctx, cursorRequests, a.Src.ListFinal, notAfter, eval); err != nil {
		return job.Outcome{}, err
	}
	orphans := func(ref string) error {
		if seen[strings.TrimPrefix(ref, "withdrawal:")] {
			return nil
		}
		ok, err := a.Src.Exists(ctx, ref)
		if err != nil || ok {
			return err // has a request: the request scan owns it
		}
		return eval(ref)
	}
	if err := a.scan(ctx, cursorSettles, a.Src.ListSettles, notAfter, orphans); err != nil {
		return job.Outcome{}, err
	}

	warn, crit, err := a.Store.CountOpen(ctx, finding.FlowWithdrawal)
	return job.Outcome{Warn: warn, Critical: crit}, err
}

type lister func(ctx context.Context, after source.Ref, notAfter time.Time, limit int) ([]source.Ref, error)

// scan walks a lister from its stored cursor to notAfter, saving the cursor after every batch so a failure
// part-way re-reads at most one batch.
func (a *WithdrawalAudit) scan(ctx context.Context, name string, list lister, notAfter time.Time, each func(ref string) error) error {
	cur, err := a.cursor(ctx, name)
	if err != nil {
		return err
	}
	for {
		refs, err := list(ctx, cur, notAfter, a.batch())
		if err != nil {
			return err
		}
		for _, r := range refs {
			if err := each(r.ID); err != nil {
				return err
			}
			cur = r
		}
		if len(refs) > 0 {
			if err := a.Store.SetCursor(ctx, name, cur.At.Format(time.RFC3339Nano)+"|"+cur.ID, a.now()); err != nil {
				return err
			}
		}
		if len(refs) < a.batch() {
			return nil
		}
	}
}

func (a *WithdrawalAudit) cursor(ctx context.Context, name string) (source.Ref, error) {
	s, err := a.Store.Cursor(ctx, name)
	if err != nil || s == "" {
		return source.Ref{At: time.Unix(0, 0).UTC()}, err
	}
	ts, id, ok := strings.Cut(s, "|")
	if !ok {
		return source.Ref{}, fmt.Errorf("corrupt cursor %q for %s", s, name)
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	return source.Ref{ID: id, At: at}, err
}
