// Package pipeline moves a reserved Withdrawal Request the rest of the way (withdrawal-flow.md
// invariants #1-#6, #16, #20; task 3.3): sign → broadcast → wait for Withdrawal Confirmation Depth →
// Settle, publishing withdrawals.broadcast / withdrawals.confirmed through the outbox.
//
// Every step is safe to repeat, so a crash anywhere just means the next poll redoes it:
//   - signing is keyed on the Withdrawal Key, and wallet-service returns the stored tx for a key it
//     already signed (#16) — a retry never allocates a second nonce;
//   - the signed raw tx is stored before broadcast, and only that exact tx is ever (re)sent;
//   - Settle and Release are idempotent on the ledger side;
//   - every state change is a guarded UPDATE ... WHERE status = <expected> (#11).
//
// It only ever moves a request FORWARD once wallet-service may have allocated a nonce for it: a
// signed tx that is dropped instead of broadcast would strand that Hot Wallet nonce and block
// every later Hot Wallet tx (the failure #16 exists to prevent).
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/pkg/events"
	"digiwal/services/withdrawal-service/internal/chain"
	"digiwal/services/withdrawal-service/internal/ledger"
	"digiwal/services/withdrawal-service/internal/store"
	"digiwal/services/withdrawal-service/internal/wallet"
)

// Failure reasons recorded on a `failed` row (reject_reason column).
const (
	ReasonSignRefused = "sign_refused" // wallet-service refused the request; no nonce allocated
	ReasonTxReverted  = "tx_reverted"  // mined, but reverted: nothing was paid out
)

type Store interface {
	ListByStatus(ctx context.Context, status string, limit int) ([]store.Withdrawal, error)
	Transition(ctx context.Context, id, from, to string, reason *string) (store.Withdrawal, error)
	MarkSigned(ctx context.Context, id string, chainID int64, nonce uint64, rawTx string) (store.Withdrawal, error)
	MarkBroadcast(ctx context.Context, id, txHash string, at time.Time, head int64, event func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error)
	MarkConfirmed(ctx context.Context, id, txHash string, block int64, ledgerTxID string, at time.Time, event func(store.Withdrawal) (string, []byte, error)) (store.Withdrawal, error)
	ListTxs(ctx context.Context, id string) ([]store.Tx, error)
	AddReplacement(ctx context.Context, id string, seq int, hash, raw string) error
	MarkSent(ctx context.Context, id string, head int64) error
	UnpublishedEvents(ctx context.Context, limit int) ([]store.OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error
}

type Ledger interface {
	Settle(ctx context.Context, key string) (ledger.Settled, error)
	Release(ctx context.Context, key string) error
}

type Signer interface {
	SignHotWallet(ctx context.Context, r wallet.SignRequest) (rawTx string, nonce uint64, err error)
	// ReplaceHotWallet asks for the same tx re-signed at the same nonce with higher fees (a Fee Bump,
	// invariant #21); the same seq replays the stored replacement.
	ReplaceHotWallet(ctx context.Context, key string, seq int) (wallet.Replacement, error)
}

type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

type Pipeline struct {
	Store     Store
	Ledger    Ledger
	Signer    Signer
	Chain     chain.Chain
	Publisher Publisher
	ChainID   int64
	// ConfirmationDepth: a tx mined in block B is final once head >= B + depth (Decided: 64 blocks;
	// configurable so a local chain needs fewer).
	ConfirmationDepth int64
	Batch             int
	Now               func() time.Time
	// Gaps enables Nonce Gap detection (task 3.4); nil disables it.
	Gaps *GapWatch

	// Recheck is the local whitelist + Screening re-check made right before signing (invariant #23);
	// nil disables it. It needs Voider: a request that fails it is only Released once wallet-service
	// has promised never to sign its key (a sign reply may have been lost, so a nonce can exist).
	Recheck func(ctx context.Context, w store.Withdrawal) (reason string, err error)
	Voider  Voider

	// Fee Bump (invariant #21; Decided: after 20 blocks, at most 5 times). Zero values mean those defaults.
	FeeBumpAfterBlocks int64
	MaxFeeBumps        int

	// Alert is where ALERTs go (default: the log, prefixed "ALERT:"), each at most every AlertEvery.
	Alert      func(msg string)
	AlertEvery time.Duration
	alerts     alerter
}

// Run polls until ctx ends. A failed step is logged and retried on the next tick.
func (p *Pipeline) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := p.Step(ctx); err != nil && ctx.Err() == nil {
			log.Printf("withdrawal pipeline: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step makes one pass over every stage. Stages are independent: one failing does not stop the
// others, and a request can advance several stages in one pass.
func (p *Pipeline) Step(ctx context.Context) error {
	return errors.Join(
		p.each(ctx, store.StatusReserved, p.sign),
		p.each(ctx, store.StatusSigned, p.broadcast),
		p.each(ctx, store.StatusBroadcast, p.confirm),
		p.each(ctx, store.StatusBroadcast, p.feeBump),
		p.publish(ctx),
		p.watchGaps(ctx),
	)
}

func (p *Pipeline) each(ctx context.Context, status string, f func(context.Context, store.Withdrawal) error) error {
	rows, err := p.Store.ListByStatus(ctx, status, p.batch())
	if err != nil {
		return fmt.Errorf("listing %s: %w", status, err)
	}
	var errs []error
	for _, w := range rows {
		if err := f(ctx, w); err != nil && !errors.Is(err, store.ErrStateChanged) {
			errs = append(errs, fmt.Errorf("%s %s: %w", status, w.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Pipeline) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return 50
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// sign: reserved → signed. A refusal (400/422: no nonce allocated) Releases the Lock and fails the
// request; a 409 (the key is already signed for a different request) is an ALERT and is never
// released, since a nonce exists under that key; a 410 means the Reaper voided the key (it is
// releasing the request itself). Anything else — wallet-service down, a Nonce Gap freeze (503) — is
// retried with the same key.
func (p *Pipeline) sign(ctx context.Context, w store.Withdrawal) error {
	if p.Recheck != nil {
		if handled, err := p.recheck(ctx, w); handled || err != nil {
			return err
		}
	}
	raw, nonce, err := p.Signer.SignHotWallet(ctx, wallet.SignRequest{
		ToAddress: w.ToAddress, AssetID: w.AssetID, Amount: w.Amount.String(), ChainID: p.ChainID, IdempotencyKey: w.Key(),
	})
	if errors.Is(err, wallet.ErrSignRefused) {
		log.Printf("withdrawal %s: %v — releasing its Lock", w.ID, err)
		return p.releaseAndFail(ctx, w, store.StatusReserved, ReasonSignRefused)
	}
	if errors.Is(err, wallet.ErrKeyVoided) {
		log.Printf("withdrawal %s: its key was voided; the Reaper releases it", w.ID)
		return nil
	}
	if errors.Is(err, wallet.ErrKeyConflict) {
		log.Printf("ALERT: withdrawal %s: %v — a nonce may be allocated under its key; not releasing", w.ID, err)
		return err
	}
	if err != nil {
		return err
	}
	if _, err := chain.TxHash(raw); err != nil {
		return fmt.Errorf("wallet-service returned an undecodable tx: %w", err)
	}
	_, err = p.Store.MarkSigned(ctx, w.ID, p.ChainID, nonce, raw)
	return err
}

// recheck is invariant #23's pre-sign check. handled reports that the request was dealt with here
// (so the caller must not sign it). A request that still passes is not handled; one that fails is
// Released and failed — but only after wallet-service voids its key, because a lost sign reply could
// mean a nonce already exists; if it had signed after all, the request simply goes on (a signed tx
// is never blocked, only cancelled by an operator's Nonce Filler).
func (p *Pipeline) recheck(ctx context.Context, w store.Withdrawal) (handled bool, err error) {
	reason, err := p.Recheck(ctx, w)
	if err != nil {
		return true, err // no verdict: sign nothing, ask again next pass
	}
	if reason == "" {
		return false, nil
	}
	if p.Voider == nil {
		p.alert(w, "recheck-no-void", fmt.Sprintf("fails the pre-sign re-check (%s) but wallet-service cannot void its key; not signing, not releasing", reason))
		return true, nil
	}
	v, err := p.Voider.VoidHotWallet(ctx, w.Key())
	if err != nil {
		return true, fmt.Errorf("voiding after a failed re-check (%s): %w", reason, err)
	}
	if v.Voided {
		log.Printf("withdrawal %s: fails the pre-sign re-check (%s) — key voided, releasing its Lock", w.ID, reason)
		return true, p.releaseAndFail(ctx, w, store.StatusReserved, reason)
	}
	if _, err := chain.TxHash(v.RawTx); err != nil {
		return true, fmt.Errorf("wallet-service returned an undecodable tx: %w", err)
	}
	p.alert(w, "recheck-already-signed", fmt.Sprintf("fails the pre-sign re-check (%s) but wallet-service had already signed it (nonce %d); it goes out unless an operator cancels it with `service hot-wallet cancel --key %s`", reason, v.Nonce, w.Key()))
	_, err = p.Store.MarkSigned(ctx, w.ID, p.ChainID, v.Nonce, v.RawTx)
	return true, err
}

func (p *Pipeline) alert(w store.Withdrawal, kind, msg string) {
	p.alerts.raise(p.now(), p.AlertEvery, p.Alert, w, kind, msg)
}

// version is one signed form of a request's tx: the original (seq 0) or a Fee Bump (invariant #21).
// All share one nonce, destination and amount; any may be mined, never more than one.
type version struct {
	seq  int
	hash common.Hash
	raw  string
}

// versions is the original tx and every Fee Bump, oldest first; the last is the one to (re)send.
func (p *Pipeline) versions(ctx context.Context, w store.Withdrawal) ([]version, error) {
	h, err := chain.TxHash(*w.SignedRawTx)
	if err != nil {
		return nil, err
	}
	out := []version{{0, h, *w.SignedRawTx}}
	txs, err := p.Store.ListTxs(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range txs {
		vh, err := chain.TxHash(t.Raw)
		if err != nil {
			return nil, err
		}
		out = append(out, version{t.Seq, vh, t.Raw})
	}
	return out, nil
}

// mined returns the version with a receipt, if any (never more than one can have one).
func (p *Pipeline) mined(ctx context.Context, vs []version) (*version, *chain.Receipt, error) {
	for i := len(vs) - 1; i >= 0; i-- {
		r, err := p.Chain.Receipt(ctx, vs[i].hash)
		if err != nil {
			return nil, nil, err
		}
		if r != nil {
			return &vs[i], r, nil
		}
	}
	return nil, nil, nil
}

// underpriced: the node's refusal that a Fee Bump fixes. Geth says "transaction underpriced" (or
// "replacement transaction underpriced" when a dearer version is already pooled); Anvil, and geth's
// state transition, say "max fee per gas less than block base fee" when the base fee rose past the
// tx's cap between signing and broadcasting (found by the e2e test).
func underpriced(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "underpriced") || strings.Contains(m, "fee too low") || strings.Contains(m, "less than block base fee")
}

// broadcast: signed → broadcast. Sends the latest stored version (never a newly built one). A node
// that already has it, or a version that is already mined, counts as sent. A node that refuses it as
// underpriced triggers a Fee Bump; the replacement goes out on the next pass.
func (p *Pipeline) broadcast(ctx context.Context, w store.Withdrawal) error {
	vs, err := p.versions(ctx, w)
	if err != nil {
		return err
	}
	latest := vs[len(vs)-1]
	if err := p.Chain.Broadcast(ctx, latest.raw); err != nil {
		if v, _, rerr := p.mined(ctx, vs); rerr != nil || v == nil {
			switch {
			case underpriced(err):
				if berr := p.bump(ctx, w, vs); berr != nil {
					log.Printf("withdrawal %s: Fee Bump after %q: %v", w.ID, err, berr)
				}
			case strings.Contains(strings.ToLower(err.Error()), "nonce too low"):
				// Our nonce was used by a different tx: this one can never land. The Reaper's proof
				// rule Releases it once it sees that on chain (invariant #17), or an operator decides.
				log.Printf("ALERT: withdrawal %s: tx %s rejected, nonce %d already used by another tx: %v", w.ID, latest.hash.Hex(), *w.Nonce, err)
			}
			return fmt.Errorf("broadcasting %s: %w", latest.hash.Hex(), err)
		}
	}
	head, err := p.Chain.Head(ctx)
	if err != nil {
		return err
	}
	at := p.now().UTC()
	_, err = p.Store.MarkBroadcast(ctx, w.ID, strings.ToLower(latest.hash.Hex()), at, head, func(w store.Withdrawal) (string, []byte, error) {
		b, err := events.Encode(events.WithdrawalBroadcast{
			Version: 1, WithdrawalID: w.ID, IdempotencyKey: w.IdempotencyKey, UserID: w.UserID, AssetID: w.AssetID,
			Amount: events.Amount(w.Amount.String()), ToAddress: w.ToAddress, ChainID: *w.ChainID,
			TxHash: *w.TxHash, Nonce: *w.Nonce, BroadcastAt: at,
		})
		return events.TopicWithdrawalsBroadcast, b, err
	})
	return err
}

// confirm: broadcast → confirmed, once a version of the tx is Withdrawal Confirmation Depth deep. The
// version mined can be a Fee Bump, not the one broadcast first: its hash becomes the request's
// tx_hash and goes out in withdrawals.confirmed. The Settle comes first — it is the only point where
// the debit Entry is posted (#3) — then the state change. A tx that was mined but reverted paid
// nothing out: its Lock is Released and the request fails.
func (p *Pipeline) confirm(ctx context.Context, w store.Withdrawal) error {
	vs, err := p.versions(ctx, w)
	if err != nil {
		return err
	}
	v, r, err := p.mined(ctx, vs)
	if err != nil || v == nil {
		return err // not mined yet, or an RPC error
	}
	txHash := strings.ToLower(v.hash.Hex())
	head, err := p.Chain.Head(ctx)
	if err != nil {
		return err
	}
	if head < r.BlockNumber+p.ConfirmationDepth {
		return nil
	}
	if !r.Success {
		// Decided (Tech Lead): only once the revert is itself Withdrawal Confirmation Depth deep (a
		// shallower receipt can still change under a reorg); Release, fail, no auto-retry. The revert
		// reason is recorded; a Hot Wallet short of the asset is our fault, not the user's: ALERT.
		reason, rerr := p.Chain.RevertReason(ctx, v.hash, r.BlockNumber)
		if rerr != nil {
			reason = "unknown (" + rerr.Error() + ")"
		}
		level := "withdrawal"
		if HotWalletShort(reason) {
			level = "ALERT: withdrawal"
		}
		log.Printf("%s %s: tx %s reverted in block %d, revert reason %q — releasing its Lock", level, w.ID, txHash, r.BlockNumber, reason)
		return p.releaseAndFail(ctx, w, store.StatusBroadcast, ReasonTxReverted)
	}
	settled, err := p.Ledger.Settle(ctx, w.Key())
	if errors.Is(err, ledger.ErrConflict) || errors.Is(err, ledger.ErrLockNotFound) {
		// The Lock was released (or never existed) although the tx paid out: the user got the money
		// and kept the balance. Never paper over it automatically.
		log.Printf("ALERT: withdrawal %s: tx %s confirmed but Settle failed: %v — needs an operator", w.ID, txHash, err)
		return err
	}
	if err != nil {
		return err
	}
	at := p.now().UTC()
	_, err = p.Store.MarkConfirmed(ctx, w.ID, txHash, r.BlockNumber, settled.LedgerTransactionID, at, func(w store.Withdrawal) (string, []byte, error) {
		b, err := events.Encode(events.WithdrawalConfirmed{
			Version: 1, WithdrawalID: w.ID, IdempotencyKey: w.IdempotencyKey, UserID: w.UserID, AssetID: w.AssetID,
			Amount: events.Amount(w.Amount.String()), ToAddress: w.ToAddress, ChainID: *w.ChainID,
			TxHash: *w.TxHash, Nonce: *w.Nonce, BlockNumber: head, ConfirmedAt: at,
		})
		return events.TopicWithdrawalsConfirmed, b, err
	})
	return err
}

func (p *Pipeline) bumpAfter() int64 {
	if p.FeeBumpAfterBlocks > 0 {
		return p.FeeBumpAfterBlocks
	}
	return 20
}

func (p *Pipeline) maxBumps() int {
	if p.MaxFeeBumps > 0 {
		return p.MaxFeeBumps
	}
	return 5
}

// feeBump is the broadcast-stage half of invariant #21: re-send a version that was never sent, and
// replace a tx that has not been mined 20 blocks after it was sent while it is underpriced against
// the current base fee. Idempotent and crash-safe: the replacement is stored (sent_head NULL = "not
// sent yet") before anything is broadcast, and asking wallet-service for the same seq replays it.
func (p *Pipeline) feeBump(ctx context.Context, w store.Withdrawal) error {
	vs, err := p.versions(ctx, w)
	if err != nil {
		return err
	}
	if v, _, err := p.mined(ctx, vs); err != nil || v != nil {
		return err // mined: confirm() takes it from here
	}
	head, err := p.Chain.Head(ctx)
	if err != nil {
		return err
	}
	latest := vs[len(vs)-1]
	if w.SentHead == nil { // a Fee Bump was stored and has not gone out yet
		if err := p.Chain.Broadcast(ctx, latest.raw); err != nil {
			if underpriced(err) { // already replaced-by-fee at the node by something dearer: bump again
				return errors.Join(err, p.bump(ctx, w, vs))
			}
			return fmt.Errorf("sending Fee Bump %d: %w", latest.seq, err)
		}
		return p.Store.MarkSent(ctx, w.ID, head)
	}
	if head-*w.SentHead < p.bumpAfter() {
		return nil
	}
	tx, err := decodeRaw(latest.raw)
	if err != nil {
		return err
	}
	base, err := p.Chain.BaseFee(ctx)
	if err != nil {
		return err
	}
	if tx.GasFeeCap().Cmp(base) >= 0 {
		return nil // not mined yet, but not underpriced: a bump would only cost more
	}
	return p.bump(ctx, w, vs)
}

// bump asks wallet-service for the next Fee Bump version and stores it. At most MaxFeeBumps; then, or
// when wallet-service refuses (fee ceiling, a Nonce Filler is the latest version), an ALERT.
func (p *Pipeline) bump(ctx context.Context, w store.Withdrawal, vs []version) error {
	n := len(vs) - 1 // Fee Bumps so far
	if n >= p.maxBumps() {
		p.alert(w, "bump-limit", fmt.Sprintf("still not mined after %d Fee Bumps (nonce %d); not bumping again, an operator must decide", n, *w.Nonce))
		return nil
	}
	rep, err := p.Signer.ReplaceHotWallet(ctx, w.Key(), n+1)
	switch {
	case errors.Is(err, wallet.ErrReplaceCeiling):
		p.alert(w, "bump-ceiling", fmt.Sprintf("underpriced, but Fee Bump %d would exceed wallet-service's fee ceiling: %v", n+1, err))
		return nil
	case errors.Is(err, wallet.ErrReplaceRefused):
		p.alert(w, "bump-refused", fmt.Sprintf("wallet-service refused Fee Bump %d: %v", n+1, err))
		return nil
	case err != nil:
		return err // transient (a Nonce Gap freeze, wallet-service down): ask again with the same seq
	}
	if err := sameTx(vs[0].raw, rep.RawTx, *w.Nonce); err != nil {
		// Defence in depth: wallet-service builds replacements from the stored original, so this
		// cannot happen; if it ever does, nothing is stored or sent.
		p.alert(w, "bump-mismatch", fmt.Sprintf("wallet-service's Fee Bump %d is not the same tx: %v; not stored, not sent", n+1, err))
		return nil
	}
	h, _ := chain.TxHash(rep.RawTx)
	return p.Store.AddReplacement(ctx, w.ID, n+1, strings.ToLower(h.Hex()), rep.RawTx)
}

// HotWalletShort reports whether a revert message says the sender lacked funds — for a withdrawal
// that is the Hot Wallet running short of the asset, an operational fault that ops must fix.
func HotWalletShort(revertReason string) bool {
	m := strings.ToLower(revertReason)
	for _, s := range []string{"insufficient balance", "exceeds balance", "insufficient funds", "transfer amount exceeds"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// releaseAndFail gives the Lock back, then marks the request failed. Release first: if the
// transition then fails, the next pass Releases again (idempotent) and retries the transition.
func (p *Pipeline) releaseAndFail(ctx context.Context, w store.Withdrawal, from, reason string) error {
	return releaseAndFail(ctx, p.Store, p.Ledger, w, from, reason)
}

type transitioner interface {
	Transition(ctx context.Context, id, from, to string, reason *string) (store.Withdrawal, error)
}

func releaseAndFail(ctx context.Context, st transitioner, lg Ledger, w store.Withdrawal, from, reason string) error {
	if err := lg.Release(ctx, w.Key()); err != nil {
		return fmt.Errorf("releasing: %w", err)
	}
	_, err := st.Transition(ctx, w.ID, from, store.StatusFailed, &reason)
	return err
}

// publish sends outbox events Kafka has not acknowledged yet, in order, and marks each published
// only after the acknowledgement. Stops at the first failure so order is kept.
func (p *Pipeline) publish(ctx context.Context) error {
	evs, err := p.Store.UnpublishedEvents(ctx, p.batch())
	if err != nil {
		return fmt.Errorf("reading outbox: %w", err)
	}
	for _, e := range evs {
		if err := p.Publisher.Publish(ctx, e.Topic, e.WithdrawalID, e.Payload); err != nil {
			return err
		}
		if err := p.Store.MarkPublished(ctx, e.ID); err != nil {
			return err
		}
	}
	return nil
}

func decodeRaw(raw string) (*types.Transaction, error) { return chain.Decode(raw) }

// sameTx checks that a replacement is the original with only its fees changed: same chain, nonce,
// destination, value, data and gas, and the same sender.
func sameTx(origRaw, repRaw string, nonce int64) error {
	o, err := decodeRaw(origRaw)
	if err != nil {
		return err
	}
	r, err := decodeRaw(repRaw)
	if err != nil {
		return err
	}
	origSender, err := chain.Sender(origRaw)
	if err != nil {
		return err
	}
	rs, err := chain.Sender(repRaw)
	if err != nil {
		return err
	}
	switch {
	case r.Nonce() != o.Nonce() || int64(r.Nonce()) != nonce:
		return fmt.Errorf("nonce %d, want %d", r.Nonce(), nonce)
	case rs != origSender:
		return fmt.Errorf("sender %s, want %s", rs.Hex(), origSender.Hex())
	case r.To() == nil || o.To() == nil || *r.To() != *o.To():
		return errors.New("destination differs")
	case r.Value().Cmp(o.Value()) != 0:
		return errors.New("value differs")
	case !bytes.Equal(r.Data(), o.Data()):
		return errors.New("data differs")
	case r.ChainId().Cmp(o.ChainId()) != 0:
		return errors.New("chain id differs")
	case r.Gas() != o.Gas():
		return errors.New("gas limit differs")
	case r.GasFeeCap().Cmp(o.GasFeeCap()) <= 0:
		return errors.New("max fee did not go up")
	}
	return nil
}
