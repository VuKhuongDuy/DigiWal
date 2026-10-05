package audit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/deposit"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

// DepositAudit applies D1-D7 (docs/design/reconciliation-audit.md Decision (a)). Same shape as
// WithdrawalAudit: open and pending references are re-evaluated every run, observed transfers are audited
// once they have been confirmed for the Grace Period, and a reference is evaluated at most once per run.
// Two further scans find what has no starting row: ledger credits with no observed transfer (D2), and
// on-chain transfers to a deposit address that chain-watcher never recorded (D7).
type DepositAudit struct {
	Src     *source.Deposits
	Store   *store.Store
	ChainID int64

	Every      time.Duration // default 5m
	Grace      time.Duration // default 2 runs
	Escalate   time.Duration // warn -> critical for D6, default 3 runs
	Batch      int
	StartBlock int64 // where the D7 scan starts on its first run (Open Question 4: default block 0)
	ScanBlocks int64 // blocks per D7 batch
	Now        func() time.Time
}

const (
	cursorObserved = "deposit_audit"
	cursorCredits  = "deposit_orphan_credits"
	cursorBlocks   = "deposit_missed_scan"
)

func (a *DepositAudit) Name() string { return "deposit_audit" }

func (a *DepositAudit) Interval() time.Duration {
	if a.Every > 0 {
		return a.Every
	}
	return 5 * time.Minute
}

func (a *DepositAudit) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *DepositAudit) grace() time.Duration {
	if a.Grace > 0 {
		return a.Grace
	}
	return 2 * a.Interval()
}

func (a *DepositAudit) escalate() time.Duration {
	if a.Escalate > 0 {
		return a.Escalate
	}
	return 3 * a.Interval()
}

func (a *DepositAudit) batch() int {
	if a.Batch > 0 {
		return a.Batch
	}
	return 200
}

func (a *DepositAudit) scanBlocks() int64 {
	if a.ScanBlocks > 0 {
		return a.ScanBlocks
	}
	return 200
}

func (a *DepositAudit) Run(ctx context.Context) (job.Outcome, error) {
	runID := job.RunID(ctx)
	now := a.now()
	notAfter := now.Add(-a.grace())
	seen := map[string]bool{}

	eval := func(ref string) error {
		if seen[ref] {
			return nil
		}
		seen[ref] = true
		f, err := a.Src.Gather(ctx, ref)
		if err != nil {
			return fmt.Errorf("reading %s: %w", ref, err)
		}
		return a.Store.Reconcile(ctx, runID, finding.FlowDeposit, ref, deposit.Rules, deposit.Evaluate(f), now, a.escalate())
	}

	open, err := a.Store.OpenRefs(ctx, finding.FlowDeposit)
	if err != nil {
		return job.Outcome{}, err
	}
	for _, ref := range open {
		if err := eval(ref); err != nil {
			return job.Outcome{}, err
		}
	}
	if err := scanRefs(ctx, a.Store, cursorObserved, a.Src.ListConfirmed, notAfter, a.batch(), a.now, eval); err != nil {
		return job.Outcome{}, err
	}
	credits := func(ref string) error {
		if seen[ref] {
			return nil
		}
		ok, err := a.Src.Observed(ctx, ref)
		if err != nil || ok {
			return err // observed: the observed-transfer scan owns it
		}
		return eval(ref)
	}
	if err := scanRefs(ctx, a.Store, cursorCredits, a.Src.ListLedgerDeposits, notAfter, a.batch(), a.now, credits); err != nil {
		return job.Outcome{}, err
	}
	if err := a.scanMissed(ctx, notAfter, func(ref string) error {
		if seen[ref] {
			return nil
		}
		ok, err := a.Src.Observed(ctx, ref)
		if err != nil || ok {
			return err
		}
		return eval(ref)
	}); err != nil {
		return job.Outcome{}, err
	}

	warn, crit, err := a.Store.CountOpen(ctx, finding.FlowDeposit)
	return job.Outcome{Warn: warn, Critical: crit}, err
}

// scanMissed walks the chain for transfers to deposit addresses (D7). It only looks at blocks chain-watcher
// says it has scanned (up to its watermark) and that are older than the Grace Period, so a transfer the
// watcher has not got to yet is not "missed". Its cursor is a block number.
func (a *DepositAudit) scanMissed(ctx context.Context, notAfter time.Time, each func(ref string) error) error {
	wm, ok, err := a.Src.Watermark(ctx, a.ChainID)
	if err != nil || !ok {
		return err // chain-watcher has not scanned anything yet
	}
	cur, err := a.Store.Cursor(ctx, cursorBlocks)
	if err != nil {
		return err
	}
	next := a.StartBlock
	if cur != "" {
		last, err := strconv.ParseInt(cur, 10, 64)
		if err != nil {
			return fmt.Errorf("corrupt cursor %q for %s", cur, cursorBlocks)
		}
		next = last + 1
	}
	tokens, addrs, err := a.Src.ScanTargets(ctx)
	if err != nil {
		return err
	}
	isDeposit := func(addr string) bool { return addrs[addr] }

	for next <= wm {
		end := next + a.scanBlocks() - 1
		if end > wm {
			end = wm
		}
		end, err = graceEnd(ctx, a.Src.Chain, next, end, notAfter)
		if err != nil {
			return err
		}
		if end < next {
			return nil // the next block is still inside the Grace Period
		}
		cands, err := a.Src.Chain.Scan(ctx, next, end, tokens, isDeposit)
		if err != nil {
			return err
		}
		for _, c := range cands {
			if a.Src.Hot != "" && c.LogIndex == -1 && strings.EqualFold(c.From, a.Src.Hot) {
				continue
			}
			if err := each(source.DepositKey(a.ChainID, c.TxHash, c.LogIndex)); err != nil {
				return err
			}
		}
		if err := a.Store.SetCursor(ctx, cursorBlocks, strconv.FormatInt(end, 10), a.now()); err != nil {
			return err
		}
		next = end + 1
	}
	return nil
}
