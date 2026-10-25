package audit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/treasury"
	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

// TreasuryAudit scans the chain for every transfer with a Hot, Warm or Cold address on one side, records it in
// treasury_movements, and applies T1-T5 (docs/design/reconciliation-audit.md "Treasury and Hot Wallet
// signing"). Warm and Cold are moved by an offline CLI that records nothing, so the chain is the only source.
// All rules are Immediate: they compare the chain with signatures wallet-service wrote before it broadcast.
//
// Hot Wallet nonce state is per Hot address: T1, T2 and T4 reconcile the CURRENT Hot address (Tiers.Hot, which
// the auditor learns from wallet-service's system-addresses) and never mix in another address's counter; T3
// looks at every signature. See source.hotScope for how pre-migration (hot_address is empty) rows are attributed.
//
// The tier addresses come from configuration, not from wallet-service: the auditor calls no service API, and
// the system-addresses endpoint is withdrawal-service's (task A8 adds Cold and the auditor there).
type TreasuryAudit struct {
	Src     *source.Treasury
	Store   *store.Store
	Tiers   treasury.Addresses
	ChainID int64

	Every      time.Duration // default 5m
	Grace      time.Duration // blocks must be this old, default 2 runs
	Confirms   int64         // and this many blocks deep, default 2
	StartBlock int64
	ScanBlocks int64
	Now        func() time.Time
}

const cursorTreasury = "treasury_scan"

func (a *TreasuryAudit) Name() string { return "treasury_audit" }

func (a *TreasuryAudit) Interval() time.Duration {
	if a.Every > 0 {
		return a.Every
	}
	return 5 * time.Minute
}

func (a *TreasuryAudit) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *TreasuryAudit) grace() time.Duration {
	if a.Grace > 0 {
		return a.Grace
	}
	return 2 * a.Interval()
}

func (a *TreasuryAudit) scanBlocks() int64 {
	if a.ScanBlocks > 0 {
		return a.ScanBlocks
	}
	return 200
}

func (a *TreasuryAudit) confirms() int64 {
	if a.Confirms > 0 {
		return a.Confirms
	}
	return 2
}

func (a *TreasuryAudit) Run(ctx context.Context) (job.Outcome, error) {
	runID, now := job.RunID(ctx), a.now()
	byRef := map[string][]finding.Violation{} // refs fully recomputed this run: absence means resolved
	touched := map[string]bool{}              // refs evaluated this run
	add := func(ref string, v *finding.Violation) {
		touched[ref] = true
		if v != nil {
			byRef[ref] = append(byRef[ref], *v)
		}
	}

	if !strings.EqualFold(a.Src.Hot, a.Tiers.Hot) {
		return job.Outcome{}, fmt.Errorf("treasury audit: source scoped to Hot %q but tiers say %q", a.Src.Hot, a.Tiers.Hot)
	}

	// T1 (per Hot address, key-custody-hsm.md invariant 24): read the chain first, then the database (next_nonce only grows, so this order cannot false-alarm).
	nonceRef := treasury.NonceRef(a.ChainID, a.Tiers.Hot)
	count, err := a.Src.Chain.TxCount(ctx, a.Tiers.Hot)
	if err != nil {
		return job.Outcome{}, err
	}
	next, ok, err := a.Src.NextNonce(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	if !ok {
		next = 0
	}
	add(nonceRef, treasury.T1(count, next))

	// T3: every signature, each run.
	sigs, err := a.Src.Signatures(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	for _, s := range sigs {
		add(treasury.SigRef(s.Key), treasury.T3(s))
	}

	// T2, T4, T5 over newly scanned blocks.
	if err := a.scanBlocks_(ctx, now, add); err != nil {
		return job.Outcome{}, err
	}

	// Open references of the "tx:" kind that were not in the scanned range are re-checked one by one (a reorg
	// can remove the tx). "mv:" references (T5) are facts about the past and are never re-evaluated.
	open, err := a.Store.OpenRefs(ctx, finding.FlowTreasury)
	if err != nil {
		return job.Outcome{}, err
	}
	known, err := a.Src.Known(ctx)
	if err != nil {
		return job.Outcome{}, err
	}
	for _, ref := range open {
		switch {
		case touched[ref]:
		case strings.HasPrefix(ref, "sig:"):
			touched[ref] = true // fully recomputed above and not violating now
		case strings.HasPrefix(ref, "nonce:"):
			// "nonce:<chain>:<hot>" was recomputed above only if it is the current Hot's (touched, handled
			// first). One for a previous Hot address stays open: nobody re-checks that address, so it must not
			// be resolved by silence. The old "nonce:<chain>" form (before per-address state) was the one Hot
			// address of its day and is resolved here like the current one.
			if len(strings.Split(ref, ":")) == 2 {
				touched[ref] = true
			}
		case strings.HasPrefix(ref, "tx:"):
			parts := strings.Split(ref, ":")
			if len(parts) != 3 {
				continue
			}
			found, fromHot, nonce, err := a.Src.Chain.Sent(ctx, parts[2], a.Tiers.Hot)
			if err != nil {
				return job.Outcome{}, err
			}
			if !found || !fromHot {
				add(ref, nil) // no longer on chain, or never ours to check
				continue
			}
			add(ref, treasury.CheckHotTx(treasury.HotTx{Hash: parts[2], Nonce: nonce, HasMovement: true}, known))
		}
	}

	openSet := map[string]bool{}
	for _, ref := range open {
		openSet[ref] = true
	}
	for ref := range touched {
		if len(byRef[ref]) == 0 && !openSet[ref] {
			continue // nothing found and nothing to resolve
		}
		if err := a.Store.Reconcile(ctx, runID, finding.FlowTreasury, ref, treasury.Rules, byRef[ref], now, 0); err != nil {
			return job.Outcome{}, err
		}
	}
	warn, crit, err := a.Store.CountOpen(ctx, finding.FlowTreasury)
	return job.Outcome{Warn: warn, Critical: crit}, err
}

func (a *TreasuryAudit) scanBlocks_(ctx context.Context, now time.Time, add func(string, *finding.Violation)) error {
	head, err := a.Src.Chain.Head(ctx)
	if err != nil {
		return err
	}
	limit := head - a.confirms()
	cur, err := a.Store.Cursor(ctx, cursorTreasury)
	if err != nil {
		return err
	}
	next := a.StartBlock
	if cur != "" {
		last, err := strconv.ParseInt(cur, 10, 64)
		if err != nil {
			return fmt.Errorf("corrupt cursor %q for %s", cur, cursorTreasury)
		}
		next = last + 1
	}
	if next > limit {
		return nil
	}
	known, err := a.Src.Known(ctx)
	if err != nil {
		return err
	}
	tokens, native, err := a.Src.Assets(ctx)
	if err != nil {
		return err
	}
	deposits, err := a.Src.DepositAddresses(ctx)
	if err != nil {
		return err
	}
	isDeposit := func(addr string) bool { return deposits[addr] }
	var tokenList []string
	for t := range tokens {
		tokenList = append(tokenList, t)
	}
	var tiers []string
	for _, t := range []string{a.Tiers.Hot, a.Tiers.Warm, a.Tiers.Cold} {
		if t != "" {
			tiers = append(tiers, t)
		}
	}

	for next <= limit {
		end := next + a.scanBlocks() - 1
		if end > limit {
			end = limit
		}
		if end, err = graceEnd(ctx, a.Src.Chain, next, end, now.Add(-a.grace())); err != nil {
			return err
		}
		if end < next {
			return nil
		}
		moves, sent, err := a.Src.Chain.ScanTreasury(ctx, next, end, a.Tiers.Hot, tiers, tokenList)
		if err != nil {
			return err
		}

		// which Hot txs moved value out
		outbound := map[string]bool{}
		var batch []treasury.Movement
		for _, r := range moves {
			m := treasury.Movement{ChainID: a.ChainID, TxHash: r.TxHash, LogIndex: r.LogIndex, Token: r.Token, Amount: r.Amount,
				From: r.From, To: r.To, Block: r.Block, Time: r.Time}
			m.AssetID = native
			if r.Token != "" {
				m.AssetID = tokens[r.Token]
			}
			m.FromTier, m.ToTier = a.Tiers.TierOf(r.From, isDeposit), a.Tiers.TierOf(r.To, isDeposit)
			m.Kind = treasury.Classify(m.FromTier, m.ToTier)
			if m.FromTier == treasury.Hot && m.Amount.Sign() > 0 {
				outbound[m.TxHash] = true
			}
			batch = append(batch, m)
		}

		unexplained := map[string]bool{}
		for _, h := range sent {
			ref := treasury.TxRef(a.ChainID, h.Hash)
			v := treasury.CheckHotTx(treasury.HotTx{Hash: h.Hash, Nonce: h.Nonce, HasMovement: outbound[h.Hash]}, known)
			add(ref, v)
			if v != nil && v.Rule == "T4" {
				unexplained[h.Hash] = true
			}
		}
		for _, m := range batch {
			if unexplained[m.TxHash] && m.FromTier == treasury.Hot {
				m.Kind = "unexplained"
			}
			if err := a.Store.AddMovement(ctx, store.Movement{ChainID: m.ChainID, TxHash: m.TxHash, LogIndex: m.LogIndex, Asset: m.AssetID,
				Amount: m.Amount.String(), From: m.From, To: m.To, FromTier: m.FromTier, ToTier: m.ToTier, Kind: m.Kind, Block: m.Block, Time: m.Time}); err != nil {
				return err
			}
			add(m.Ref(), treasury.T5(m))
		}
		if err := a.Store.SetCursor(ctx, cursorTreasury, strconv.FormatInt(end, 10), a.now()); err != nil {
			return err
		}
		next = end + 1
	}
	return nil
}
