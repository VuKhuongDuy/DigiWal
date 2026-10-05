package audit

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/custody"
	"digiwal/services/auditor/internal/rules/treasury"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

// Reconciliation compares, per asset, what the chain holds at a recent block with what the ledger says it holds
// (docs/design/reconciliation-audit.md "Aggregate reconciliation"): it catches funds that no per-transaction
// rule looks for. Order matters: the chain is read first, at block B, and the databases after, so the moves
// that happen between the reads can only make the books newer than the chain, which is exactly what the
// "explained" terms cover. B = head - Confirms, and Confirms must not exceed the deposit and withdrawal
// confirmation depths (otherwise the ledger holds credits for blocks newer than B). Whatever skew is left is
// absorbed by the two-pass rule: a residual must be seen in two consecutive runs.
type Reconciliation struct {
	Src   *source.Recon
	Store *store.Store
	Tiers treasury.Addresses

	Confirms  int64               // default 1
	Capital   map[string]*big.Int // by symbol: operator capital in custody that no user owns (RECON_CAPITAL)
	Tolerance map[string]*big.Int // by symbol: |residual| at or below this is a warn (RECON_TOLERANCE)
	Every     time.Duration
	Now       func() time.Time
}

func (a *Reconciliation) Name() string { return "reconciliation" }

func (a *Reconciliation) Interval() time.Duration {
	if a.Every > 0 {
		return a.Every
	}
	return 5 * time.Minute
}

func (a *Reconciliation) confirms() int64 {
	if a.Confirms > 0 {
		return a.Confirms
	}
	return 1
}

// tolerance: configured per symbol; else 0.01 of the native coin and 0 for a token (Objectives table).
func (a *Reconciliation) tolerance(as source.ReconAsset) *big.Int {
	if v, ok := a.Tolerance[as.Symbol]; ok {
		return v
	}
	if as.Type == "native" && as.Decimals >= 2 {
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(as.Decimals-2)), nil)
	}
	return new(big.Int)
}

func (a *Reconciliation) Run(ctx context.Context) (job.Outcome, error) {
	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now().UTC()
	}
	runID := job.RunID(ctx)

	head, err := a.Src.Chain.Head(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	block := head - a.confirms()
	if block < 0 {
		return job.Outcome{}, nil
	}
	assets, err := a.Src.Assets(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	deposits, err := a.Src.DepositAddresses(ctx)
	if err != nil {
		return job.Outcome{}, err
	}

	// 1. the chain, at block B
	type held struct{ total, hot *big.Int }
	onChain := map[string]*held{}
	for _, as := range assets {
		h := &held{new(big.Int), new(big.Int)}
		onChain[as.ID] = h
		for _, tier := range []struct {
			addr  string
			isHot bool
		}{{a.Tiers.Hot, true}, {a.Tiers.Warm, false}, {a.Tiers.Cold, false}} {
			if tier.addr == "" {
				continue
			}
			v, err := a.Src.Chain.Balance(ctx, tier.addr, as.Contract, block)
			if err != nil {
				return job.Outcome{}, err
			}
			h.total.Add(h.total, v)
			if tier.isHot {
				h.hot.Add(h.hot, v)
			}
		}
		for _, d := range deposits {
			v, err := a.Src.Chain.Balance(ctx, d, as.Contract, block)
			if err != nil {
				return job.Outcome{}, err
			}
			h.total.Add(h.total, v)
		}
	}

	// 2. the databases, after
	led, err := a.Src.LedgerFigures(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	observed, err := a.Src.ObservedUpTo(ctx, block)
	if err != nil {
		return job.Outcome{}, err
	}
	unsettled, gas, err := a.Src.InFlight(ctx, block)
	if err != nil {
		return job.Outcome{}, err
	}

	for _, as := range assets {
		lf := led[as.ID]
		if lf == nil {
			lf = &source.LedgerFigures{}
		}
		in := custody.Inputs{
			AssetID: as.ID, Symbol: as.Symbol, Block: block,
			OnChain: onChain[as.ID].total, HotOnChain: onChain[as.ID].hot,
			ChainAccount: lf.ChainAccount, UserTotal: lf.UserTotal, ActiveLocks: lf.ActiveLocks,
			ObservedUpToB: observed[as.ID], Credited: lf.Credited, FlaggedHeld: lf.FlaggedHeld,
			UnsettledWithdrawn: unsettled[as.ID], Capital: a.Capital[as.Symbol], Tolerance: a.tolerance(as),
		}
		if as.Type == "native" {
			in.UnpostedGas = gas
		}
		res := custody.Compute(in)

		comp := func(v *big.Int) string {
			if v == nil {
				return "0"
			}
			return v.String()
		}
		if err := a.Store.AddSnapshot(ctx, store.Snapshot{RunID: runID, AssetID: as.ID, Symbol: as.Symbol, Block: block,
			OnChain: comp(in.OnChain), LedgerCustody: res.LedgerCustody.String(), Explained: res.Explained.String(), Residual: res.Residual.String(), Tolerance: comp(in.Tolerance),
			Components: map[string]string{"hot": comp(in.HotOnChain), "user_balances": comp(in.UserTotal), "active_locks": comp(in.ActiveLocks),
				"observed": comp(in.ObservedUpToB), "credited": comp(in.Credited), "flagged_held": comp(in.FlaggedHeld),
				"unsettled_withdrawals": comp(in.UnsettledWithdrawn), "unposted_gas": comp(in.UnpostedGas), "capital": comp(in.Capital)},
			At: now}); err != nil {
			return job.Outcome{}, fmt.Errorf("recording the figures of %s: %w", as.Symbol, err)
		}
		if err := a.Store.Reconcile(ctx, runID, finding.FlowCustody, custody.Ref(as.ID), custody.Rules, custody.Evaluate(res), now, 0); err != nil {
			return job.Outcome{}, err
		}
	}
	warn, crit, err := a.Store.CountOpen(ctx, finding.FlowCustody)
	return job.Outcome{Warn: warn, Critical: crit}, err
}
