// Package watcher is chain-watcher's Deposit Detection loop (docs/design/deposit-flow.md).
//
// One Step does, in order:
//  1. reconcile: if the node's chain no longer contains the blocks we last scanned (a reorg
//     inside the not-yet-confirmed window), rewind the watermark to the fork point and drop
//     the pending transfers found on the abandoned branch;
//  2. scan: read new blocks, find native transfers and registered-ERC-20 Transfer logs to
//     Watched Addresses, resolve each to a user_id (Address Resolution), and record them
//     as pending — advancing the watermark in the same transaction (invariant #4);
//  3. confirm: pending transfers now Confirmation Depth blocks deep, and still in the
//     canonical chain, become confirmed (invariant #3);
//  4. publish: deposits.detected / deposits.confirmed for every row Kafka has not yet
//     acknowledged.
//
// Nothing here signs or broadcasts; chain-watcher only observes.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/chain-watcher/internal/chain"
	"digiwal/services/chain-watcher/internal/publisher"
	"digiwal/services/chain-watcher/internal/registry"
	"digiwal/services/chain-watcher/internal/resolver"
	"digiwal/services/chain-watcher/internal/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// transferTopic is keccak256("Transfer(address,address,uint256)"), ERC-20's (and ERC-721's) event.
var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// hashWindow is the deepest fork we can find: how many scanned-block hashes the store keeps.
const hashWindow = store.HashRetention

type Config struct {
	// ConfirmationDepth: blocks on top of a transfer's block before it is final
	// (Sepolia: 32, decided in deposit-flow.md; a small number against a local Anvil).
	ConfirmationDepth int64
	// BatchBlocks caps how many blocks one scan range covers.
	BatchBlocks int64
	// StartBlock is where a brand-new database starts scanning; nil = the current head.
	// Set it to the block the first address was issued at, or earlier deposits are never seen.
	StartBlock *int64
}

type Watcher struct {
	Chain     chain.Chain
	Store     *store.Store
	Resolver  resolver.Resolver
	Publisher publisher.Publisher
	// LoadRegistry is called every Step. A failed reload keeps the last good registry
	// (invariant #2: refreshed periodically, expected to change rarely).
	LoadRegistry func() (*registry.Registry, error)
	Config       Config

	reg     *registry.Registry
	chainID int64
}

// Run polls until ctx ends. A failed Step is logged and retried next tick; nothing it does
// is lost by retrying (all state is in Postgres).
func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := w.Step(ctx); err != nil && ctx.Err() == nil {
			log.Printf("chain-watcher step: %v", err)
			if errors.Is(err, store.ErrDeepReorg) {
				log.Printf("ALERT: %v — needs an operator (deposit-flow.md Open Question #4)", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step runs one full pass. Publishing is attempted even when scanning failed, since it only
// drains state that is already durable.
func (w *Watcher) Step(ctx context.Context) error {
	if w.chainID == 0 {
		id, err := w.Chain.ChainID(ctx)
		if err != nil {
			return fmt.Errorf("reading chain id: %w", err)
		}
		w.chainID = id
	}
	syncErr := w.sync(ctx)
	pubErr := w.publish(ctx)
	return errors.Join(syncErr, pubErr)
}

func (w *Watcher) sync(ctx context.Context) error {
	if err := w.refreshRegistry(); err != nil {
		return err
	}
	wm, err := w.watermark(ctx)
	if err != nil {
		return err
	}
	if wm, err = w.reconcile(ctx, wm); err != nil {
		return err
	}
	for {
		head, err := w.Chain.Head(ctx)
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
		if wm >= head {
			return w.confirm(ctx, head)
		}
		to := min(head, wm+w.batch())
		if err := w.scan(ctx, wm, to); err != nil {
			return err
		}
		wm = to
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (w *Watcher) batch() int64 {
	if w.Config.BatchBlocks > 0 {
		return w.Config.BatchBlocks
	}
	return 100
}

func (w *Watcher) refreshRegistry() error {
	r, err := w.LoadRegistry()
	if err != nil {
		if w.reg == nil {
			return err
		}
		log.Printf("asset registry reload failed, keeping the previous one: %v", err)
		return nil
	}
	w.reg = r
	return nil
}

func (w *Watcher) watermark(ctx context.Context) (int64, error) {
	wm, ok, err := w.Store.Watermark(ctx, w.chainID)
	if err != nil || ok {
		return wm, err
	}
	start := int64(-1)
	if w.Config.StartBlock != nil {
		start = *w.Config.StartBlock
	} else {
		head, err := w.Chain.Head(ctx)
		if err != nil {
			return 0, fmt.Errorf("reading head: %w", err)
		}
		start = head
	}
	// The watermark is the last block already scanned, so scanning starts at `start`.
	wm = max(start-1, 0)
	if err := w.Store.StartAt(ctx, w.chainID, wm); err != nil {
		return 0, err
	}
	log.Printf("first run on chain %d: scanning from block %d", w.chainID, wm+1)
	return wm, nil
}

// reconcile detects a reorg of the range already scanned: the block at the watermark has a
// different hash on the node than the one we recorded. A reorg replaces a suffix of the chain,
// so the tip is enough to notice; we then walk back to the newest block both agree on.
func (w *Watcher) reconcile(ctx context.Context, wm int64) (int64, error) {
	stored, ok, err := w.Store.ScannedHash(ctx, w.chainID, wm)
	if err != nil {
		return 0, err
	}
	if !ok { // nothing recorded at the watermark: a fresh start, nothing to verify
		return wm, nil
	}
	same, err := w.sameBlock(ctx, wm, stored)
	if err != nil || same {
		return wm, err
	}
	fork := int64(-1)
	for n := wm - 1; n >= 0 && n >= wm-hashWindow; n-- {
		stored, ok, err := w.Store.ScannedHash(ctx, w.chainID, n)
		if err != nil {
			return 0, err
		}
		if !ok { // before anything we recorded: the run started here
			fork = n
			break
		}
		same, err := w.sameBlock(ctx, n, stored)
		if err != nil {
			return 0, err
		}
		if same {
			fork = n
			break
		}
	}
	if fork < 0 {
		return 0, fmt.Errorf("%w: no common block within %d blocks of %d", store.ErrDeepReorg, hashWindow, wm)
	}
	log.Printf("reorg: chain no longer contains block %d as scanned; rewinding %d -> %d", wm, wm, fork)
	if err := w.Store.Rewind(ctx, w.chainID, fork); err != nil {
		return 0, err
	}
	return fork, nil
}

func (w *Watcher) sameBlock(ctx context.Context, n int64, stored string) (bool, error) {
	h, exists, err := w.Chain.BlockHash(ctx, n)
	if err != nil {
		return false, fmt.Errorf("reading block %d hash: %w", n, err)
	}
	return exists && strings.EqualFold(h.Hex(), stored), nil
}

// scan reads blocks (wm, to], records what it finds and advances the watermark to `to`.
// Any failure — including a resolution failure that is not a clean 404 (invariant #1) —
// returns before the watermark moves, so the same range is simply read again next time.
func (w *Watcher) scan(ctx context.Context, wm, to int64) error {
	from := wm + 1
	blocks := make([]*chain.Block, 0, to-wm)
	for n := from; n <= to; n++ {
		b, err := w.Chain.Block(ctx, n)
		if err != nil {
			return fmt.Errorf("reading block %d: %w", n, err)
		}
		if b == nil {
			return fmt.Errorf("node has no block %d (reorg while scanning?); retrying", n)
		}
		blocks = append(blocks, b)
	}
	// The blocks must form one chain, hanging off the block we last scanned; otherwise the
	// node reorged while we were reading and the range mixes two branches.
	if prev, ok, err := w.Store.ScannedHash(ctx, w.chainID, wm); err != nil {
		return err
	} else if ok && !strings.EqualFold(prev, blocks[0].ParentHash.Hex()) {
		return fmt.Errorf("block %d does not extend scanned block %d; reorg, retrying", from, wm)
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i].ParentHash != blocks[i-1].Hash {
			return fmt.Errorf("block %d does not extend block %d; reorg while scanning, retrying", blocks[i].Number, blocks[i-1].Number)
		}
	}
	byNumber := make(map[int64]*chain.Block, len(blocks))
	refs := make([]store.BlockRef, len(blocks))
	for i, b := range blocks {
		byNumber[b.Number] = b
		refs[i] = store.BlockRef{Number: b.Number, Hash: b.Hash.Hex()}
	}

	found := &finder{w: w, ctx: ctx, memo: map[common.Address]*owner{}}
	if w.reg.Native != "" {
		for _, b := range blocks {
			if err := found.native(b); err != nil {
				return err
			}
		}
	}
	logs, err := w.Chain.Logs(ctx, from, to, w.reg.Contracts(), transferTopic)
	if err != nil {
		return fmt.Errorf("reading logs %d-%d: %w", from, to, err)
	}
	for _, l := range logs {
		b := byNumber[l.BlockNumber]
		if b == nil || b.Hash != l.BlockHash {
			return fmt.Errorf("log at block %d belongs to a different branch than the scanned block; reorg, retrying", l.BlockNumber)
		}
		if err := found.erc20(l); err != nil {
			return err
		}
	}
	return w.Store.CommitRange(ctx, w.chainID, from, to, refs, found.out)
}

type owner struct {
	userID string
	ours   bool
}

type finder struct {
	w    *Watcher
	ctx  context.Context
	memo map[common.Address]*owner
	out  []store.Transfer
}

// resolve returns the owner of addr. A failure that is not a clean "not ours" is returned
// as an error so the caller aborts the range instead of dropping a possible deposit.
func (f *finder) resolve(addr common.Address) (*owner, error) {
	if o, ok := f.memo[addr]; ok {
		return o, nil
	}
	uid, ok, err := f.w.Resolver.Resolve(f.ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", addr, err)
	}
	o := &owner{userID: uid, ours: ok}
	f.memo[addr] = o
	return o, nil
}

func (f *finder) native(b *chain.Block) error {
	for _, tx := range b.Txs {
		if tx.To == nil || tx.Value == nil || tx.Value.Sign() <= 0 {
			continue
		}
		o, err := f.resolve(*tx.To)
		if err != nil {
			return err
		}
		if !o.ours {
			continue
		}
		// A failed transaction moves no value even though it is in the block.
		r, err := f.w.Chain.Receipt(f.ctx, tx.Hash)
		if err != nil {
			return fmt.Errorf("reading receipt %s: %w", tx.Hash, err)
		}
		if r == nil || r.BlockHash != b.Hash {
			return fmt.Errorf("receipt for %s is not in block %d as scanned; reorg, retrying", tx.Hash, b.Number)
		}
		if !r.Success {
			continue
		}
		f.out = append(f.out, store.Transfer{
			ChainID: f.w.chainID, TxHash: lowerHash(tx.Hash), LogIndex: events.NativeLogIndex,
			AssetID: f.w.reg.Native, From: tx.From.Hex(), To: tx.To.Hex(), UserID: o.userID,
			Amount: new(big.Int).Set(tx.Value), BlockNumber: b.Number, BlockHash: b.Hash.Hex(),
		})
	}
	return nil
}

func (f *finder) erc20(l chain.Log) error {
	assetID, registered := f.w.reg.ERC20[l.Address]
	if !registered { // invariant #2: never an event for an unregistered contract
		return nil
	}
	// ERC-20 Transfer(address indexed from, address indexed to, uint256 value): 3 topics, 32
	// data bytes. ERC-721's Transfer shares the signature but indexes the token id (4 topics).
	if len(l.Topics) != 3 || len(l.Data) != 32 {
		return nil
	}
	amount := new(big.Int).SetBytes(l.Data)
	if amount.Sign() == 0 {
		return nil
	}
	to := common.BytesToAddress(l.Topics[2].Bytes())
	o, err := f.resolve(to)
	if err != nil {
		return err
	}
	if !o.ours {
		return nil
	}
	f.out = append(f.out, store.Transfer{
		ChainID: f.w.chainID, TxHash: lowerHash(l.TxHash), LogIndex: l.Index,
		AssetID: assetID, From: common.BytesToAddress(l.Topics[1].Bytes()).Hex(), To: to.Hex(), UserID: o.userID,
		Amount: amount, BlockNumber: l.BlockNumber, BlockHash: l.BlockHash.Hex(),
	})
	return nil
}

func lowerHash(h common.Hash) string { return strings.ToLower(h.Hex()) }

// confirm promotes pending transfers that are Confirmation Depth blocks deep — and are
// still where we saw them — to confirmed.
func (w *Watcher) confirm(ctx context.Context, head int64) error {
	deepest := head - w.Config.ConfirmationDepth
	if deepest < 0 {
		return nil
	}
	due, err := w.Store.PendingUpTo(ctx, w.chainID, deepest)
	if err != nil {
		return err
	}
	for _, t := range due {
		r, err := w.Chain.Receipt(ctx, common.HexToHash(t.TxHash))
		if err != nil {
			return fmt.Errorf("reading receipt %s: %w", t.TxHash, err)
		}
		// Not in the canonical chain where we saw it (reorged out, or re-included elsewhere):
		// leave it pending. reconcile rewinds on the next Step and the rescan decides.
		if r == nil || !r.Success || !strings.EqualFold(r.BlockHash.Hex(), t.BlockHash) {
			log.Printf("not confirming %s:%d: no longer in block %d", t.TxHash, t.LogIndex, t.BlockNumber)
			continue
		}
		if _, err := w.Store.Confirm(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// publish drains what Kafka has not acknowledged yet: every detected event, then every
// confirmed one. A row is marked only after the broker acked, so a crash in between just
// publishes it again — at-least-once; every consumer is idempotent on the natural key.
func (w *Watcher) publish(ctx context.Context) error {
	det, err := w.Store.UnpublishedDetected(ctx, w.chainID)
	if err != nil {
		return err
	}
	for _, t := range det {
		ev := events.DepositDetected{
			Version: events.Version, ChainID: t.ChainID, TxHash: t.TxHash, LogIndex: t.LogIndex, AssetID: t.AssetID,
			FromAddress: t.From, ToAddress: t.To, UserID: t.UserID, Amount: events.AmountFromInt(t.Amount),
			BlockNumber: t.BlockNumber, DetectedAt: t.DetectedAt.UTC(),
		}
		if err := w.send(ctx, ev); err != nil {
			return err
		}
		if err := w.Store.MarkDetectedPublished(ctx, t); err != nil {
			return err
		}
	}
	conf, err := w.Store.UnpublishedConfirmed(ctx, w.chainID)
	if err != nil {
		return err
	}
	for _, t := range conf {
		ev := events.DepositConfirmed{
			Version: events.Version, ChainID: t.ChainID, TxHash: t.TxHash, LogIndex: t.LogIndex, AssetID: t.AssetID,
			FromAddress: t.From, ToAddress: t.To, UserID: t.UserID, Amount: events.AmountFromInt(t.Amount),
			BlockNumber: t.BlockNumber,
		}
		if err := w.send(ctx, ev); err != nil {
			return err
		}
		if err := w.Store.MarkConfirmedPublished(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) send(ctx context.Context, ev events.Event) error {
	body, err := events.Encode(ev)
	if err != nil {
		// Something we recorded cannot be encoded: a bug, not a transient failure. Say so loudly.
		return fmt.Errorf("encoding %s for %s: %w", ev.Topic(), ev.Key(), err)
	}
	return w.Publisher.Publish(ctx, ev.Topic(), ev.Key(), body)
}
