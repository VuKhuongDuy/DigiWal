// Package cutover is the operator tooling for the Hot cutover of key-custody-hsm.md 5.5 (M2c): moving the Hot Wallet's
// funds from the old software Hot address to the new HSM-held Hot address, signed by the old software key, with the
// signing frozen around it. It runs only inside the one-off `hot-cutover` binary (cmd/hot-cutover), never in the
// wallet-service server, and has no network surface: the freeze is a row in hot_cutover that only this tool moves.
//
// Every step is idempotent and safe to re-run. The sequence (the design's ten steps, 1 and 9 being the operator's):
//
//	preflight  refuses unless the gateway serves and its key is the expected new address
//	freeze     step 2, signing frozen (abortable)
//	settle     step 3, the old address has nothing in flight (rebroadcasts what was signed but not mined)
//	drain      steps 5-7, POINT OF NO RETURN at the plan: sign, broadcast, confirm, post the gas to the ledger
//	verify     step 8's checks, and after step 9 the final ones
//	complete   step 9's unfreeze (after the operator switched wallet-service to the HSM key and restarted consumers)
package cutover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

// GasPurpose is the Gas Fee purpose the drain's gas is posted under. The design does not name one; the ledger's
// existing set has exactly one for "our own wallet to our own wallet" (the Treasury Sweep class), which is what a
// Hot -> Hot drain is, so no ledger migration is needed.
const GasPurpose = "treasury_sweep"

const (
	transferGas  = 21000
	maxRounds    = 5 // late arrivals on the old address beyond this need an operator's eyes
	rebroadcastS = 5 // seconds between rebroadcasts of a stored tx while waiting
)

// Chain is the slice of an EVM RPC client the tool needs.
type Chain interface {
	ChainID(ctx context.Context) (*big.Int, error)
	Head(ctx context.Context) (uint64, error)
	// NonceAt is the account nonce at block (nil: latest).
	NonceAt(ctx context.Context, addr common.Address, block *big.Int) (uint64, error)
	PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error)
	BalanceAt(ctx context.Context, addr common.Address) (*big.Int, error)
	TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error)
	FeeQuote(ctx context.Context) (tip, baseFee *big.Int, err error)
	EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error)
	// Broadcast sends a signed raw tx; a node that already has it (or has mined it) is not an error.
	Broadcast(ctx context.Context, raw []byte) error
	// Receipt is nil, nil while the tx is not mined.
	Receipt(ctx context.Context, hash common.Hash) (*Receipt, error)
}

// Receipt is what the tool needs of a mined tx.
type Receipt struct {
	Success  bool
	Block    uint64
	GasUsed  uint64
	GasPrice *big.Int // effective gas price, wei
}

// Store is the slice of storage.Store the tool needs.
type Store interface {
	FreezeHotSigning(ctx context.Context, chainID int64, old, newHot string) (storage.Cutover, error)
	GetCutover(ctx context.Context, chainID int64, old string) (storage.Cutover, error)
	AbortCutover(ctx context.Context, chainID int64, old string) error
	SetCutoverState(ctx context.Context, chainID int64, old, state string) error
	NonceRow(ctx context.Context, chainID int64, hot string) (next uint64, frozenFrom *uint64, ok bool, err error)
	HotTxsFrom(ctx context.Context, chainID int64, hot string, fromNonce uint64) ([]storage.HotTx, error)
	PlanDrain(ctx context.Context, chainID int64, old string, seedNonce func(context.Context) (uint64, error),
		build func(firstNonce uint64) ([]storage.DrainPlan, error)) (bool, error)
	Drains(ctx context.Context, chainID int64, old string) ([]storage.Drain, error)
	MarkDrainBroadcast(ctx context.Context, key string) error
	MarkDrainConfirmed(ctx context.Context, key string, gasUsed uint64, price *big.Int) error
	MarkDrainGasPosted(ctx context.Context, key string) error
}

// GatewayKey is hsm-gateway's GET /v1/hot/key, as far as the tool cares.
type GatewayKey struct {
	Address common.Address
	ChainID uint64
	Serving bool
}

// Gateway reads the new Hot key from hsm-gateway.
type Gateway interface {
	Key(ctx context.Context) (GatewayKey, error)
}

// Legacy is the OLD Hot key, the software one (index 0 of the hot_wallet seed). Only this binary ever holds it
// after the cutover is planned, and only to sign drain transactions.
type Legacy interface {
	Address() common.Address
	SignTx(tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

// ErrLedgerRefused: ledger-service will never accept this Gas Fee as sent (400, or 409: the same tx was already
// reported with different values). Retrying cannot help.
var ErrLedgerRefused = errors.New("ledger-service refused the Gas Fee")

// GasFee is a mined tx's gas as its receipt reports it.
type GasFee struct {
	ChainID  int64
	TxHash   string
	GasUsed  uint64
	GasPrice *big.Int
	Purpose  string
}

// Ledger posts a Gas Fee (POST /v1/ledger/gas-fees). The ledger derives the idempotency key from the chain and tx
// hash (gas:<chain>:<hash>), so a repeat is harmless.
type Ledger interface {
	PostGasFee(ctx context.Context, g GasFee) error
}

// Config is what a run needs.
type Config struct {
	ChainID       int64
	Assets        signing.Assets
	NewHot        common.Address // the address the operator expects, as the ceremony printed it
	Confirmations uint64         // blocks on top of a tx before it counts as confirmed
	Poll          time.Duration
	Timeout       time.Duration // how long settle and drain wait for the chain
	FeeCeiling    *big.Int      // wei: the cap on the drain's gas price (the decided 50 gwei)
	DryRun        bool
	Out           io.Writer
	// Sleep waits d (tests advance a fake chain here). Defaults to a context-aware time.Sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Tool runs the steps.
type Tool struct {
	cfg    Config
	chain  Chain
	store  Store
	gw     Gateway
	legacy Legacy
	ledger Ledger
}

// New returns a Tool.
func New(cfg Config, chain Chain, store Store, gw Gateway, legacy Legacy, ledger Ledger) *Tool {
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.FeeCeiling == nil {
		cfg.FeeCeiling = signing.DefaultFeeCeiling
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	return &Tool{cfg: cfg, chain: chain, store: store, gw: gw, legacy: legacy, ledger: ledger}
}

func (t *Tool) say(format string, a ...any) { fmt.Fprintf(t.cfg.Out, format+"\n", a...) }

// would prints a dry-run line.
func (t *Tool) would(format string, a ...any) { t.say("[dry-run] would "+format, a...) }

func lower(a common.Address) string { return strings.ToLower(a.Hex()) }

// Preflight refuses to go on unless the chain, the gateway and the two addresses are what the operator expects.
// It changes nothing and is part of every other step.
func (t *Tool) Preflight(ctx context.Context) error {
	id, err := t.chain.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("reading the chain id from the RPC: %w", err)
	}
	if id.Int64() != t.cfg.ChainID {
		return fmt.Errorf("the RPC is chain %d, the tool was told chain %d", id, t.cfg.ChainID)
	}
	if t.cfg.NewHot == (common.Address{}) {
		return errors.New("the expected new Hot address is required (the one the HSM ceremony printed)")
	}
	k, err := t.gw.Key(ctx)
	if err != nil {
		return fmt.Errorf("hsm-gateway GET /v1/hot/key: %w (the HSM backend must be up before the cutover)", err)
	}
	if !k.Serving {
		return errors.New("hsm-gateway is not serving (self-test not passed, or it fails closed): refusing to start; fix the gateway first")
	}
	if k.ChainID != id.Uint64() {
		return fmt.Errorf("hsm-gateway is pinned to chain %d, the RPC is chain %d", k.ChainID, id)
	}
	if k.Address != t.cfg.NewHot {
		return fmt.Errorf("hsm-gateway's Hot key is %s, but the expected new Hot address is %s: refusing (wrong token, wrong environment or a typo)", k.Address, t.cfg.NewHot)
	}
	old := t.legacy.Address()
	switch {
	case old == (common.Address{}):
		return errors.New("the old Hot address is zero")
	case old == k.Address:
		return fmt.Errorf("the old and the new Hot address are the same (%s): there is nothing to cut over", old)
	}
	t.say("preflight ok: chain %d, old Hot %s (software), new Hot %s (hsm-gateway, serving)", id, old, k.Address)
	return nil
}

// Freeze is step 2: new Hot signing returns 503 hot_cutover from now on. Already-signed keys still replay and
// Fee Bump / Cancel / Void keep working with the old key.
func (t *Tool) Freeze(ctx context.Context) error {
	if err := t.Preflight(ctx); err != nil {
		return err
	}
	old := t.legacy.Address()
	if t.cfg.DryRun {
		t.would("freeze new Hot signing on chain %d (old Hot %s -> new Hot %s)", t.cfg.ChainID, old, t.cfg.NewHot)
		return nil
	}
	c, err := t.store.FreezeHotSigning(ctx, t.cfg.ChainID, lower(old), lower(t.cfg.NewHot))
	if err != nil {
		return err
	}
	switch c.State {
	case storage.CutoverDone:
		t.say("this cutover is already done (%s)", c.DoneAt.UTC().Format(time.RFC3339))
	default:
		t.say("signing is frozen on chain %d since %s (cutover state: %s)", c.ChainID, c.FrozenAt.UTC().Format(time.RFC3339), c.State)
	}
	return nil
}

func (t *Tool) open(ctx context.Context) (storage.Cutover, error) {
	c, err := t.store.GetCutover(ctx, t.cfg.ChainID, lower(t.legacy.Address()))
	if errors.Is(err, storage.ErrNotFound) {
		return c, errors.New("signing is not frozen for this cutover: run freeze first")
	}
	if err != nil {
		return c, err
	}
	if c.NewHot != lower(t.cfg.NewHot) {
		return c, fmt.Errorf("%w: %s", storage.ErrCutoverMismatch, c.NewHot)
	}
	return c, nil
}

// inflight is what stops the old address from being settled.
type inflight struct {
	pending, latest, confirmed, allocated uint64
	rows                                  []storage.HotTx
}

func (i inflight) settled() bool {
	return i.confirmed == i.allocated && i.latest == i.allocated && i.pending == i.allocated
}

func (t *Tool) readInflight(ctx context.Context, old common.Address) (inflight, error) {
	var in inflight
	head, err := t.chain.Head(ctx)
	if err != nil {
		return in, err
	}
	var blk *big.Int
	if head >= t.cfg.Confirmations {
		blk = new(big.Int).SetUint64(head - t.cfg.Confirmations)
	} else {
		blk = big.NewInt(0)
	}
	if in.confirmed, err = t.chain.NonceAt(ctx, old, blk); err != nil {
		return in, err
	}
	if in.latest, err = t.chain.NonceAt(ctx, old, nil); err != nil {
		return in, err
	}
	if in.pending, err = t.chain.PendingNonceAt(ctx, old); err != nil {
		return in, err
	}
	next, frozenFrom, ok, err := t.store.NonceRow(ctx, t.cfg.ChainID, lower(old))
	if err != nil {
		return in, err
	}
	if frozenFrom != nil {
		return in, fmt.Errorf("a Nonce Gap is open on the old Hot address (frozen from nonce %d): resolve it first (withdrawal-flow.md invariants 7-9, `service hot-wallet cancel`), then re-run", *frozenFrom)
	}
	if ok {
		in.allocated = next
	}
	if in.latest > in.allocated {
		return in, fmt.Errorf("the chain has seen %d txs from the old Hot address but wallet-service allocated only %d nonces: something signed with the old key outside wallet-service (auditor T1). Stop and investigate", in.latest, in.allocated)
	}
	if in.rows, err = t.store.HotTxsFrom(ctx, t.cfg.ChainID, lower(old), in.latest); err != nil {
		return in, err
	}
	return in, nil
}

// Settle is step 3: wait until every nonce wallet-service allocated for the old Hot address is mined and Confirmations
// deep, with nothing pending. A tx that was signed but is not in any mempool (the sweeper was stopped between sign and
// broadcast, a node dropped it) is rebroadcast from the stored signature, latest version first; a tx that is stuck
// below the base fee is for Fee Bump (withdrawal-service does it on its own) or `service hot-wallet cancel`: this
// tool never signs a replacement itself.
func (t *Tool) Settle(ctx context.Context) error {
	if err := t.Preflight(ctx); err != nil {
		return err
	}
	if _, err := t.open(ctx); err != nil {
		return err
	}
	old := t.legacy.Address()
	deadline := time.Now().Add(t.cfg.Timeout)
	var last string
	lastSent := map[string]time.Time{}
	for {
		in, err := t.readInflight(ctx, old)
		if err != nil {
			return err
		}
		if in.settled() {
			t.say("settled: the old Hot address has %d txs, all %d confirmed, none pending", in.allocated, t.cfg.Confirmations)
			return nil
		}
		line := fmt.Sprintf("waiting: allocated %d, confirmed-deep %d, mined %d, pending %d; %d signed tx(s) not mined", in.allocated, in.confirmed, in.latest, in.pending, len(in.rows))
		if line != last {
			t.say("%s", line)
			last = line
		}
		if t.cfg.DryRun {
			for _, r := range in.rows {
				t.would("rebroadcast nonce %d (key %s, signed for %s, %d version(s))", r.Nonce, r.Key, r.CallerCN, len(r.Versions))
			}
			t.would("wait up to %s for the chain to settle", t.cfg.Timeout)
			return nil
		}
		for _, r := range in.rows {
			if time.Since(lastSent[r.Key]) < rebroadcastS*time.Second {
				continue
			}
			lastSent[r.Key] = time.Now()
			// The latest version is the one most likely to be accepted; "nonce too low" just means a version was mined.
			if err := t.chain.Broadcast(ctx, r.Versions[len(r.Versions)-1]); err != nil {
				t.say("  rebroadcast of nonce %d (%s): %v", r.Nonce, r.Key, err)
			}
		}
		if time.Now().After(deadline) {
			var b strings.Builder
			for _, r := range in.rows {
				fmt.Fprintf(&b, "\n  nonce %d key %s (signed for %s): not mined; Fee Bump it or `service hot-wallet cancel --key %s`", r.Nonce, r.Key, r.CallerCN, r.Key)
			}
			return fmt.Errorf("timed out after %s: the old Hot address still has %d nonce(s) in flight:%s", t.cfg.Timeout, in.allocated-in.confirmed, b.String())
		}
		if err := t.cfg.Sleep(ctx, t.cfg.Poll); err != nil {
			return err
		}
	}
}

// ---- drain -----------------------------------------------------------------------------------

type tokenAsset struct {
	id       string
	contract common.Address
}

// layout is the registered assets: the one native id and the ERC-20s in id order.
func (t *Tool) layout() (native string, tokens []tokenAsset, err error) {
	ids := make([]string, 0, len(t.cfg.Assets))
	for id := range t.cfg.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := t.cfg.Assets[id]
		switch a.Type {
		case signing.AssetNative:
			if native != "" {
				return "", nil, errors.New("the asset registry has more than one native asset")
			}
			native = id
		case signing.AssetERC20:
			tokens = append(tokens, tokenAsset{id: id, contract: a.Contract})
		}
	}
	if native == "" {
		return "", nil, errors.New("the asset registry has no native asset")
	}
	return native, tokens, nil
}

// price is the one gas price (fee cap = tip) every drain tx pays: the effective price then equals it exactly, so
// the native drain can leave the old address with nothing. Capped at the Fee Ceiling.
func (t *Tool) price(ctx context.Context) (*big.Int, error) {
	tip, base, err := t.chain.FeeQuote(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching fees: %w", err)
	}
	p := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
	if p.Cmp(t.cfg.FeeCeiling) > 0 {
		p = new(big.Int).Set(t.cfg.FeeCeiling)
	}
	if p.Sign() == 0 {
		p = big.NewInt(1)
	}
	return p, nil
}

func erc20Transfer(to common.Address, amount *big.Int) []byte {
	data := []byte{0xa9, 0x05, 0x9c, 0xbb}
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	return append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
}

type drainTx struct {
	asset    string
	amount   *big.Int
	to       common.Address
	value    *big.Int
	data     []byte
	gasLimit uint64
}

// sign builds and signs one drain tx with the old key at nonce, and re-checks the sender.
func (t *Tool) sign(d drainTx, nonce uint64, price *big.Int) ([]byte, string, error) {
	to := d.to
	unsigned := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(t.cfg.ChainID), Nonce: nonce, GasTipCap: price, GasFeeCap: price,
		Gas: d.gasLimit, To: &to, Value: d.value, Data: d.data})
	signed, err := t.legacy.SignTx(unsigned, big.NewInt(t.cfg.ChainID))
	if err != nil {
		return nil, "", fmt.Errorf("signing the drain with the old key: %w", err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(big.NewInt(t.cfg.ChainID)), signed)
	if err != nil || sender != t.legacy.Address() {
		return nil, "", fmt.Errorf("the drain signature recovers to %s, not the old Hot address %s", sender, t.legacy.Address())
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	return raw, strings.ToLower(signed.Hash().Hex()), nil
}

func isDustNative(bal, price *big.Int) bool {
	return bal.Cmp(new(big.Int).Mul(big.NewInt(transferGas), price)) <= 0
}

// Drain is steps 5 to 7. Planning (signing the drain txs with the old key and storing them) is THE POINT OF NO
// RETURN: from the commit on, the drain exists as signed transactions that any re-run broadcasts, and the freeze can
// no longer be aborted, only completed. Then it broadcasts, waits for Confirmations, and posts each tx's gas to the
// ledger. ERC-20 balances go first (the old address pays their gas), the native balance goes last in the same round,
// computed after the ERC-20 drains are mined so the old address ends at exactly zero. If funds land on the old
// address meanwhile (a late Sweep) it plans another round.
func (t *Tool) Drain(ctx context.Context) error {
	if err := t.Preflight(ctx); err != nil {
		return err
	}
	c, err := t.open(ctx)
	if err != nil {
		return err
	}
	if c.State == storage.CutoverDone {
		t.say("the cutover is done; nothing to drain")
		return nil
	}
	old := t.legacy.Address()
	native, tokens, err := t.layout()
	if err != nil {
		return err
	}
	if c.State == storage.CutoverFrozen {
		// Before the point of no return the old address must be settled: everything below depends on its nonce.
		in, err := t.readInflight(ctx, old)
		if err != nil {
			return err
		}
		if !in.settled() {
			return errors.New("the old Hot address still has txs in flight: run settle first")
		}
	}
	if err := t.postTopUpGas(ctx, old); err != nil {
		return err
	}

	deadline := time.Now().Add(t.cfg.Timeout)
	for {
		ds, err := t.store.Drains(ctx, t.cfg.ChainID, lower(old))
		if err != nil {
			return err
		}
		cur := 0
		for _, d := range ds {
			if d.Round > cur {
				cur = d.Round
			}
		}
		rows := rowsOf(ds, cur)
		hasNative := false
		for _, d := range rows {
			hasNative = hasNative || d.AssetID == native
		}

		if cur == 0 || (hasNative && allDone(rows)) {
			// Nothing in progress: is there anything (more) to drain?
			if cur >= maxRounds {
				return fmt.Errorf("funds keep arriving on the old Hot address after %d rounds: stop the senders (sweeper) and investigate", maxRounds)
			}
			planned, err := t.planRound(ctx, old, cur+1, native, tokens)
			if err != nil {
				return err
			}
			if t.cfg.DryRun {
				return nil
			}
			if !planned {
				return t.markDrained(ctx, ds)
			}
			continue
		}
		// Something is planned: execute what is not done, then plan the round's native tx (the ERC-20s are mined now).
		if err := t.execute(ctx, rows, deadline); err != nil {
			return err
		}
		if t.cfg.DryRun {
			return nil
		}
		if !hasNative {
			planned, err := t.planNative(ctx, old, cur, native)
			if err != nil {
				return err
			}
			if !planned { // only dust is left: the round is complete without a native tx
				if planned, err = t.planRound(ctx, old, cur+1, native, tokens); err != nil {
					return err
				}
				if !planned {
					return t.markDrained(ctx, ds)
				}
			}
		}
	}
}

func rowsOf(ds []storage.Drain, round int) []storage.Drain {
	var out []storage.Drain
	for _, d := range ds {
		if d.Round == round {
			out = append(out, d)
		}
	}
	return out
}

func allDone(rows []storage.Drain) bool {
	for _, d := range rows {
		if d.ConfirmedAt == nil || d.GasPostedAt == nil {
			return false
		}
	}
	return true
}

func (t *Tool) markDrained(ctx context.Context, ds []storage.Drain) error {
	if t.cfg.DryRun {
		return nil
	}
	if err := t.store.SetCutoverState(ctx, t.cfg.ChainID, lower(t.legacy.Address()), storage.CutoverDrained); err != nil {
		return err
	}
	t.say("drained: the old Hot address is empty (%d drain tx(s) in total); the Hot address may now be switched", len(ds))
	return nil
}

// postTopUpGas reports to the ledger the gas of every Gas Top-Up the old Hot address sent (key `gas-topup:...`). The
// sweeper posts it itself once a top-up is mined, but the procedure stops the sweeper while a top-up may be in flight,
// and a restarted sweeper does not post one that was mined while it was down (the deposit's native balance is already
// enough, so it never repeats the step). Left alone that is a drift between the chain and the ledger that the auditor's
// reconciliation reports. The ledger keys a Gas Fee by chain and tx hash, so re-reporting one the sweeper did post is a no-op.
func (t *Tool) postTopUpGas(ctx context.Context, old common.Address) error {
	txs, err := t.store.HotTxsFrom(ctx, t.cfg.ChainID, lower(old), 0)
	if err != nil {
		return err
	}
	posted, pending := 0, 0
	for _, tx := range txs {
		if !strings.HasPrefix(tx.Key, "gas-topup:") {
			continue
		}
		// Any version (the original or a Fee Bump) may be the one that was mined.
		for i := len(tx.Versions) - 1; i >= 0; i-- {
			var raw types.Transaction
			if err := raw.UnmarshalBinary(tx.Versions[i]); err != nil {
				return fmt.Errorf("stored Gas Top-Up %s is undecodable: %w", tx.Key, err)
			}
			rc, err := t.chain.Receipt(ctx, raw.Hash())
			if err != nil {
				return fmt.Errorf("reading the receipt of %s: %w", raw.Hash(), err)
			}
			if rc == nil {
				continue
			}
			if !rc.Success {
				t.say("  Gas Top-Up %s reverted on chain: its gas is posted too", raw.Hash())
			}
			if t.cfg.DryRun {
				pending++
				break
			}
			err = t.ledger.PostGasFee(ctx, GasFee{ChainID: t.cfg.ChainID, TxHash: strings.ToLower(raw.Hash().Hex()), GasUsed: rc.GasUsed, GasPrice: rc.GasPrice, Purpose: "gas_topup"})
			switch {
			case errors.Is(err, ErrLedgerRefused):
				t.say("  ledger-service keeps its own record of the gas of %s (%v)", raw.Hash(), err)
			case err != nil:
				return fmt.Errorf("posting the gas of Gas Top-Up %s to the ledger: %w", raw.Hash(), err)
			default:
				posted++
			}
			break
		}
	}
	switch {
	case t.cfg.DryRun && pending > 0:
		t.would("post to the ledger the gas of %d mined Gas Top-Up(s) of the old Hot address (idempotent on the tx hash)", pending)
	case posted > 0:
		t.say("gas of %d Gas Top-Up(s) of the old Hot address confirmed in the ledger (the sweeper was stopped while they were in flight)", posted)
	}
	return nil
}

// planRound plans the ERC-20 part of a round, or the native tx when there is no ERC-20 balance. false: nothing to drain.
func (t *Tool) planRound(ctx context.Context, old common.Address, round int, native string, tokens []tokenAsset) (bool, error) {
	price, err := t.price(ctx)
	if err != nil {
		return false, err
	}
	var txs []drainTx
	gasNeeded := new(big.Int)
	for _, tok := range tokens {
		bal, err := t.chain.TokenBalance(ctx, tok.contract, old)
		if err != nil {
			return false, fmt.Errorf("reading the old Hot balance of %s: %w", tok.id, err)
		}
		if bal.Sign() == 0 {
			continue
		}
		data := erc20Transfer(t.cfg.NewHot, bal)
		gas, err := t.chain.EstimateGas(ctx, old, tok.contract, new(big.Int), data)
		if err != nil {
			return false, fmt.Errorf("estimating the gas of draining %s: %w", tok.id, err)
		}
		gas = gas * 12 / 10 // the same headroom as every ERC-20 tx here
		txs = append(txs, drainTx{asset: tok.id, amount: bal, to: tok.contract, value: new(big.Int), data: data, gasLimit: gas})
		gasNeeded.Add(gasNeeded, new(big.Int).Mul(new(big.Int).SetUint64(gas), price))
	}
	if len(txs) == 0 {
		return t.planNative(ctx, old, round, native)
	}
	nat, err := t.chain.BalanceAt(ctx, old)
	if err != nil {
		return false, err
	}
	need := new(big.Int).Add(gasNeeded, new(big.Int).Mul(big.NewInt(transferGas), price))
	if nat.Cmp(need) < 0 {
		return false, fmt.Errorf("the old Hot address has %s wei but the drain needs %s wei of gas (%d ERC-20 transfer(s) and the final native one at %s wei/gas): send at least %s wei to %s and re-run (nothing was signed)",
			nat, need, len(txs), price, new(big.Int).Sub(need, nat), old)
	}
	if t.cfg.DryRun {
		for _, d := range txs {
			t.would("drain %s: %s units to %s (gas limit %d, price %s wei), signed with the OLD key  [POINT OF NO RETURN]", d.asset, d.amount, t.cfg.NewHot, d.gasLimit, price)
		}
		t.would("then drain the native balance (%s wei now, minus gas) last, after those are mined", nat)
		return true, nil
	}
	return t.plan(ctx, old, round, price, txs)
}

// planNative plans the native drain of the round from the balance now. false: only dust is left.
func (t *Tool) planNative(ctx context.Context, old common.Address, round int, native string) (bool, error) {
	price, err := t.price(ctx)
	if err != nil {
		return false, err
	}
	bal, err := t.chain.BalanceAt(ctx, old)
	if err != nil {
		return false, err
	}
	if isDustNative(bal, price) {
		if t.cfg.DryRun {
			t.would("skip the native drain: %s wei is below the gas of one transfer", bal)
		}
		return false, nil
	}
	amount := new(big.Int).Sub(bal, new(big.Int).Mul(big.NewInt(transferGas), price))
	d := drainTx{asset: native, amount: amount, to: t.cfg.NewHot, value: amount, gasLimit: transferGas}
	if t.cfg.DryRun {
		t.would("drain native: %s wei to %s (balance %s minus %d gas at %s wei), signed with the OLD key  [POINT OF NO RETURN]", amount, t.cfg.NewHot, bal, transferGas, price)
		return true, nil
	}
	return t.plan(ctx, old, round, price, []drainTx{d})
}

// plan signs txs at consecutive nonces and stores them all under the nonce-row lock (the point of no return).
func (t *Tool) plan(ctx context.Context, old common.Address, round int, price *big.Int, txs []drainTx) (bool, error) {
	t.say("POINT OF NO RETURN: signing %d drain tx(s) of round %d with the old key", len(txs), round)
	planned, err := t.store.PlanDrain(ctx, t.cfg.ChainID, lower(old),
		func(ctx context.Context) (uint64, error) { return t.chain.PendingNonceAt(ctx, old) },
		func(first uint64) ([]storage.DrainPlan, error) {
			// The chain must agree with the counter: the first drain nonce is the next one the chain would accept.
			if pending, err := t.chain.PendingNonceAt(ctx, old); err != nil {
				return nil, err
			} else if pending != first {
				return nil, fmt.Errorf("the old Hot address's next nonce is %d on chain but %d in wallet-service: run settle", pending, first)
			}
			var out []storage.DrainPlan
			for i, d := range txs {
				raw, hash, err := t.sign(d, first+uint64(i), price)
				if err != nil {
					return nil, err
				}
				out = append(out, storage.DrainPlan{Key: storage.DrainKey(t.cfg.ChainID, lower(old), d.asset, round), AssetID: d.asset, Round: round, Amount: d.amount, Raw: raw, Hash: hash})
			}
			return out, nil
		})
	if err != nil {
		return false, err
	}
	if planned {
		for _, d := range txs {
			t.say("  planned: drain %s, %s units -> %s", d.asset, d.amount, t.cfg.NewHot)
		}
	}
	return planned, nil
}

// execute broadcasts every drain tx that is not confirmed, waits for it, and posts its gas.
func (t *Tool) execute(ctx context.Context, rows []storage.Drain, deadline time.Time) error {
	for _, d := range rows {
		if t.cfg.DryRun {
			state := "to broadcast"
			if d.ConfirmedAt != nil {
				state = "confirmed"
			}
			t.would("continue drain tx %s (%s, nonce %d, %s units): %s", d.TxHash, d.AssetID, d.Nonce, d.Amount, state)
			continue
		}
		if d.ConfirmedAt == nil {
			if err := t.confirm(ctx, d, deadline); err != nil {
				return err
			}
			d, _ = t.refresh(ctx, d.Key)
		}
		if d.GasPostedAt == nil && d.ConfirmedAt != nil {
			if err := t.postGas(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tool) refresh(ctx context.Context, key string) (storage.Drain, error) {
	ds, err := t.store.Drains(ctx, t.cfg.ChainID, lower(t.legacy.Address()))
	if err != nil {
		return storage.Drain{}, err
	}
	for _, d := range ds {
		if d.Key == key {
			return d, nil
		}
	}
	return storage.Drain{}, fmt.Errorf("drain %s vanished", key)
}

func (t *Tool) confirm(ctx context.Context, d storage.Drain, deadline time.Time) error {
	hash := common.HexToHash(d.TxHash)
	var lastSend time.Time
	for {
		if time.Since(lastSend) >= rebroadcastS*time.Second || lastSend.IsZero() {
			lastSend = time.Now()
			if err := t.chain.Broadcast(ctx, d.Raw); err != nil {
				t.say("  broadcast of %s: %v", d.TxHash, err)
			} else if err := t.store.MarkDrainBroadcast(ctx, d.Key); err != nil {
				return err
			}
		}
		rc, err := t.chain.Receipt(ctx, hash)
		if err != nil {
			return fmt.Errorf("reading the receipt of %s: %w", d.TxHash, err)
		}
		if rc != nil {
			if !rc.Success {
				return fmt.Errorf("drain tx %s REVERTED in block %d: stop; the old Hot address keeps what it had; investigate before anything else", d.TxHash, rc.Block)
			}
			head, err := t.chain.Head(ctx)
			if err != nil {
				return err
			}
			if head >= rc.Block+t.cfg.Confirmations {
				// A tx never pays more than its fee cap per gas. The drain sets tip == cap, and Anvil's receipt then reports
				// base fee + tip, above the cap and above what it charged; the ledger must get what the chain charged.
				price := rc.GasPrice
				var sent types.Transaction
				if sent.UnmarshalBinary(d.Raw) == nil && price.Cmp(sent.GasFeeCap()) > 0 {
					price = new(big.Int).Set(sent.GasFeeCap())
				}
				if err := t.store.MarkDrainConfirmed(ctx, d.Key, rc.GasUsed, price); err != nil {
					return err
				}
				t.say("  confirmed: %s (%s, nonce %d, block %d, gas %d at %s wei)", d.TxHash, d.AssetID, d.Nonce, rc.Block, rc.GasUsed, price)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for drain tx %s: re-run drain (it rebroadcasts the same signed tx)", d.TxHash)
		}
		if err := t.cfg.Sleep(ctx, t.cfg.Poll); err != nil {
			return err
		}
	}
}

// postGas is step 7: the drain's gas into the ledger, so the auditor's reconciliation of the system wallets against
// the ledger's Chain Account sees no drift.
func (t *Tool) postGas(ctx context.Context, d storage.Drain) error {
	if err := t.ledger.PostGasFee(ctx, GasFee{ChainID: t.cfg.ChainID, TxHash: d.TxHash, GasUsed: d.GasUsed, GasPrice: d.EffectiveGasPrice, Purpose: GasPurpose}); err != nil {
		return fmt.Errorf("posting the gas of %s to the ledger: %w (re-run drain: it is idempotent on the tx hash)", d.TxHash, err)
	}
	if err := t.store.MarkDrainGasPosted(ctx, d.Key); err != nil {
		return err
	}
	t.say("  gas of %s in the ledger (%d x %s wei)", d.TxHash, d.GasUsed, d.EffectiveGasPrice)
	return nil
}

// ---- verify, complete, abort ------------------------------------------------------------------

// Check is one line of verify.
type Check struct {
	Name string
	OK   bool
	Info string
}

// Verify runs the checks of steps 8 and 9 and returns them; it errors if any failed. Read-only.
func (t *Tool) Verify(ctx context.Context) ([]Check, error) {
	var checks []Check
	add := func(name string, ok bool, format string, a ...any) {
		checks = append(checks, Check{name, ok, fmt.Sprintf(format, a...)})
		mark := "ok  "
		if !ok {
			mark = "FAIL"
		}
		t.say("%s %s: %s", mark, name, fmt.Sprintf(format, a...))
	}
	if err := t.Preflight(ctx); err != nil {
		return checks, err
	}
	c, err := t.open(ctx)
	if err != nil {
		return checks, err
	}
	old := t.legacy.Address()
	native, tokens, err := t.layout()
	if err != nil {
		return checks, err
	}
	add("cutover state", c.State == storage.CutoverDrained || c.State == storage.CutoverDone, "%s", c.State)

	ds, err := t.store.Drains(ctx, t.cfg.ChainID, lower(old))
	if err != nil {
		return checks, err
	}
	undone := 0
	sums := map[string]*big.Int{}
	for _, d := range ds {
		if d.ConfirmedAt == nil || d.GasPostedAt == nil {
			undone++
		}
		if sums[d.AssetID] == nil {
			sums[d.AssetID] = new(big.Int)
		}
		sums[d.AssetID].Add(sums[d.AssetID], d.Amount)
	}
	add("drain txs confirmed and their gas in the ledger", undone == 0, "%d tx(s), %d not done", len(ds), undone)

	price, err := t.price(ctx)
	if err != nil {
		return checks, err
	}
	nat, err := t.chain.BalanceAt(ctx, old)
	if err != nil {
		return checks, err
	}
	add("old Hot native balance is empty", isDustNative(nat, price), "%s wei (dust threshold %s)", nat, new(big.Int).Mul(big.NewInt(transferGas), price))
	for _, tok := range tokens {
		bal, err := t.chain.TokenBalance(ctx, tok.contract, old)
		if err != nil {
			return checks, err
		}
		add("old Hot "+tok.id+" balance is empty", bal.Sign() == 0, "%s", bal)
	}

	newNat, err := t.chain.BalanceAt(ctx, t.cfg.NewHot)
	if err != nil {
		return checks, err
	}
	want := sums[native]
	if want == nil {
		want = new(big.Int)
	}
	add("new Hot holds the drained native balance", newNat.Cmp(want) >= 0, "%s wei (drained %s)", newNat, want)
	for _, tok := range tokens {
		bal, err := t.chain.TokenBalance(ctx, tok.contract, t.cfg.NewHot)
		if err != nil {
			return checks, err
		}
		w := sums[tok.id]
		if w == nil {
			w = new(big.Int)
		}
		add("new Hot holds the drained "+tok.id, bal.Cmp(w) >= 0, "%s (drained %s)", bal, w)
	}

	in, err := t.readInflight(ctx, old)
	if err != nil {
		add("old Hot nonce row", false, "%v", err)
	} else {
		add("old Hot nonce row = chain nonce, nothing pending", in.settled() && in.allocated == in.latest, "next_nonce %d, chain nonce %d, pending %d", in.allocated, in.latest, in.pending)
	}
	next, frozenFrom, ok, err := t.store.NonceRow(ctx, t.cfg.ChainID, lower(t.cfg.NewHot))
	if err != nil {
		return checks, err
	}
	latest, err := t.chain.NonceAt(ctx, t.cfg.NewHot, nil)
	if err != nil {
		return checks, err
	}
	switch {
	case !ok:
		add("new Hot nonce row", true, "none yet (it is seeded from the chain's pending nonce on the first signature)")
	default:
		add("new Hot nonce row is its own, not the old counter", frozenFrom == nil && next >= latest, "next_nonce %d, chain nonce %d", next, latest)
	}

	var failed []string
	for _, ch := range checks {
		if !ch.OK {
			failed = append(failed, ch.Name)
		}
	}
	if len(failed) > 0 {
		return checks, fmt.Errorf("verify failed: %s", strings.Join(failed, "; "))
	}
	t.say("verify ok")
	return checks, nil
}

// Complete is step 9's unfreeze. The operator has switched wallet-service to the HSM key (SIGNER_BACKEND=pkcs11) and
// restarted the consumers of GET /v1/wallet/hot-address; the tool cannot see that, so it requires the affirmation
// and re-verifies everything it can first.
func (t *Tool) Complete(ctx context.Context, switched bool) error {
	if !switched {
		return errors.New("refusing to unfreeze without -switched: first set wallet-service to SIGNER_BACKEND=pkcs11 and restart it, then sweeper, chain-watcher and withdrawal-service (runbook step 8-9)")
	}
	c, err := t.open(ctx)
	if err != nil {
		return err
	}
	if c.State == storage.CutoverDone {
		t.say("already unfrozen (done %s)", c.DoneAt.UTC().Format(time.RFC3339))
		return nil
	}
	if c.State != storage.CutoverDrained {
		return fmt.Errorf("%w: the cutover is %s; finish drain first", storage.ErrCutoverState, c.State)
	}
	if _, err := t.Verify(ctx); err != nil {
		return err
	}
	if t.cfg.DryRun {
		t.would("unfreeze Hot signing on chain %d (the new Hot key signs from then on)", t.cfg.ChainID)
		return nil
	}
	if err := t.store.SetCutoverState(ctx, t.cfg.ChainID, lower(t.legacy.Address()), storage.CutoverDone); err != nil {
		return err
	}
	t.say("unfrozen: Hot signing resumes on the new Hot address %s", t.cfg.NewHot)
	return nil
}

// Abort lifts the freeze before the point of no return; after it, it refuses.
func (t *Tool) Abort(ctx context.Context) error {
	if t.cfg.DryRun {
		t.would("abort the cutover and unfreeze (only possible before the drain is planned)")
		return nil
	}
	if err := t.store.AbortCutover(ctx, t.cfg.ChainID, lower(t.legacy.Address())); err != nil {
		return err
	}
	t.say("aborted: signing is unfrozen and the old Hot key is still the Hot key")
	return nil
}
