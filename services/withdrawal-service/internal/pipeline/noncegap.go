package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/store"
)

// Nonce Gap detection (task 3.4, withdrawal-flow.md invariants #7-#9, #19). A `confirmed` request's
// tx can still be taken back by a reorg deeper than Withdrawal Confirmation Depth. If it is, its
// Hot Wallet nonce N is missing from the canonical chain and, by EVM nonce ordering, no Hot Wallet
// tx at N or above can confirm. So:
//  1. every request at nonce >= N holding a tx becomes `disputed` (one transaction with the
//     recorded gap) — only an operator resolves those (#9);
//  2. wallet-service is told to stop allocating Hot Wallet nonces from N on (#7, #19), retried
//     until it acknowledges.

type GapStore interface {
	ConfirmedSince(ctx context.Context, chainID, minBlock int64) ([]store.Withdrawal, error)
	OpenNonceGap(ctx context.Context, chainID, fromNonce int64, txHash string) ([]string, error)
	UnreportedNonceGaps(ctx context.Context) ([]store.NonceGap, error)
	MarkNonceGapReported(ctx context.Context, g store.NonceGap) error
}

type GapReporter interface {
	ReportNonceGap(ctx context.Context, chainID int64, fromNonce uint64) error
}

// GapWatch is the detector's configuration and its one piece of memory.
type GapWatch struct {
	Store    GapStore
	Reporter GapReporter
	// Window: confirmed txs mined in the last Window blocks are re-checked every pass. A reorg
	// deeper than that is not looked for.
	Window int64
	// missing counts consecutive passes a confirmed tx had no receipt. A gap is declared only on
	// the second miss in a row, so one flaky RPC answer cannot freeze all Hot Wallet signing.
	missing map[string]int
}

func (p *Pipeline) watchGaps(ctx context.Context) error {
	g := p.Gaps
	if g == nil {
		return nil
	}
	if g.missing == nil {
		g.missing = map[string]int{}
	}
	head, err := p.Chain.Head(ctx)
	if err != nil {
		return err
	}
	rows, err := g.Store.ConfirmedSince(ctx, p.ChainID, head-g.Window)
	if err != nil {
		return fmt.Errorf("listing confirmed: %w", err)
	}
	var gap *store.Withdrawal
	seen := map[string]bool{}
	for i := range rows {
		w := rows[i]
		seen[w.ID] = true
		r, err := p.Chain.Receipt(ctx, common.HexToHash(*w.TxHash))
		if err != nil {
			return err
		}
		if r != nil {
			delete(g.missing, w.ID)
			continue
		}
		g.missing[w.ID]++
		if g.missing[w.ID] >= 2 && (gap == nil || *w.Nonce < *gap.Nonce) {
			gap = &w
		}
	}
	for id := range g.missing { // forget rows that left the window or the status
		if !seen[id] {
			delete(g.missing, id)
		}
	}
	if gap != nil {
		ids, err := g.Store.OpenNonceGap(ctx, p.ChainID, *gap.Nonce, *gap.TxHash)
		if err != nil {
			return fmt.Errorf("recording nonce gap: %w", err)
		}
		log.Printf("ALERT: Nonce Gap on chain %d at nonce %d: confirmed tx %s (withdrawal %s) is no longer in the canonical chain; disputed %d request(s): %s — needs an operator (invariant #9)",
			p.ChainID, *gap.Nonce, *gap.TxHash, gap.ID, len(ids), strings.Join(ids, ", "))
		delete(g.missing, gap.ID)
	}
	return p.reportGaps(ctx)
}

func (p *Pipeline) reportGaps(ctx context.Context) error {
	gaps, err := p.Gaps.Store.UnreportedNonceGaps(ctx)
	if err != nil {
		return err
	}
	for _, gp := range gaps {
		if err := p.Gaps.Reporter.ReportNonceGap(ctx, gp.ChainID, uint64(gp.FromNonce)); err != nil {
			return fmt.Errorf("reporting nonce gap %d/%d to wallet-service: %w", gp.ChainID, gp.FromNonce, err)
		}
		if err := p.Gaps.Store.MarkNonceGapReported(ctx, gp); err != nil {
			return err
		}
		log.Printf("wallet-service froze Hot Wallet signing on chain %d from nonce %d", gp.ChainID, gp.FromNonce)
	}
	return nil
}
