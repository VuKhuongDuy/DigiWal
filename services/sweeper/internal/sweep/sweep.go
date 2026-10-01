// Package sweep moves a confirmed deposit from the user's attribution address to the Hot Wallet
// (docs/design/deposit-flow.md, Sweep and Gas Top-Up). sweeper holds no key: wallet-service signs
// both the Gas Top-Up and the Sweep, sweeper only decides, broadcasts and waits.
//
// The design's idempotency is "by construction": nothing is remembered between attempts. Every
// attempt re-reads live chain state (invariants #8, #10), so a redelivered event, a crash after
// broadcast or a retry after a timeout all converge: whatever was already moved is not moved
// again. A Sweep never touches the Ledger (invariant #9): failing here costs custody
// housekeeping, never a user's balance.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/sweeper/internal/chain"
	"digiwal/services/sweeper/internal/ledger"
	"digiwal/services/sweeper/internal/registry"
	"digiwal/services/sweeper/internal/wallet"

	"github.com/ethereum/go-ethereum/common"
)

// ErrPermanent marks an event that can never be swept as it stands (wrong chain, unknown asset,
// wallet-service refusing). Retrying it would only wedge the partition.
var ErrPermanent = errors.New("cannot be swept")

type Screener interface{ Contains(address string) bool }

// Wallet is the slice of wallet-service the Sweep uses.
type Wallet interface {
	SignHotWallet(ctx context.Context, req wallet.HotRequest) (rawTx []byte, nonce uint64, err error)
	SignUserAddress(ctx context.Context, req wallet.UserRequest) (rawTx []byte, err error)
	// LookupHotKey and ReplaceHotWallet are a stuck Gas Top-Up's Fee Bump (task 2.8, withdrawal-flow.md #21).
	LookupHotKey(ctx context.Context, key string) (*wallet.HotKey, error)
	ReplaceHotWallet(ctx context.Context, key string, seq int) (rawTx []byte, txHash string, err error)
}

// GasPoster reports the gas a mined tx of ours spent to ledger-service (ledger.md invariant #12).
type GasPoster interface {
	PostGasFee(ctx context.Context, g ledger.GasFee) error
}

type Config struct {
	ChainID int64
	// HotWallet is the Sweep's destination. wallet-service enforces it independently
	// (invariant #7), so a wrong value here is refused there, never honoured.
	HotWallet common.Address
	// GasTopUp is what a Gas Top-Up sends (deposit-flow.md: 0.001 ETH).
	GasTopUp *big.Int
	// GasReserve is the native balance below which a user address cannot pay for an ERC-20
	// Sweep and gets a Gas Top-Up (65,000 gas x 10 gwei = 0.00065 ETH in the design's math).
	GasReserve *big.Int
	// InclusionTimeout bounds each wait for a broadcast transaction to be mined; PollInterval
	// is how often the chain is asked.
	InclusionTimeout time.Duration
	PollInterval     time.Duration
	// Fee Bump of a stuck Gas Top-Up (Decided: after 20 blocks while underpriced, at most 5 times; the
	// 50 gwei ceiling is wallet-service's). Zero values mean those defaults.
	FeeBumpAfterBlocks int64
	MaxFeeBumps        int
}

type Sweeper struct {
	Chain     chain.Chain
	Wallet    Wallet
	Assets    registry.Assets
	Screening Screener
	Config    Config
	// Ledger receives each mined Gas Top-Up's and Sweep's gas; nil disables it. Best effort: sweeper keeps
	// no state, so a crash between a tx being mined and its Gas Fee being posted loses that fee — an accepted
	// limitation (ledger.md Open Questions #6, Decided).
	Ledger GasPoster
	// Alert is where ALERTs go; default: the log, prefixed "ALERT:".
	Alert func(msg string)
}

// TopUpKey is the Gas Top-Up's idempotency key (deposit-flow.md invariant #13, PROPOSED):
// derived from the deposit's own natural key, so a redelivered event recomputes the same one.
func TopUpKey(chainID int64, txHash string, logIndex int64) string {
	return fmt.Sprintf("gas-topup:%d:%s:%d", chainID, lower(txHash), logIndex)
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Sweep handles one deposits.confirmed event. nil means done (moved, or nothing to move);
// an error wrapping ErrPermanent will never succeed; any other error is transient.
func (s *Sweeper) Sweep(ctx context.Context, ev events.DepositConfirmed) error {
	// Invariant #11 / compliance-screening.md #2: checked independently of ledger-service, so a
	// flagged deposit's funds are never mixed into the Hot Wallet.
	if s.Screening.Contains(ev.FromAddress) {
		log.Printf("deposit %s not swept: sender %s is on the Screening List", ev.Key(), ev.FromAddress)
		return nil
	}
	if ev.ChainID != s.Config.ChainID {
		return fmt.Errorf("%w: event is for chain %d, sweeper runs on %d", ErrPermanent, ev.ChainID, s.Config.ChainID)
	}
	asset, ok := s.Assets.Lookup(ev.AssetID)
	if !ok {
		return fmt.Errorf("%w: asset %s is not in the registry", ErrPermanent, ev.AssetID)
	}
	user := common.HexToAddress(ev.ToAddress)
	if err := s.waitNoPending(ctx, user); err != nil {
		return err
	}
	switch asset.Type {
	case registry.AssetNative:
		return s.sweepNative(ctx, ev, user)
	case registry.AssetERC20:
		return s.sweepToken(ctx, ev, user, asset)
	default:
		return fmt.Errorf("%w: asset %s has type %q", ErrPermanent, ev.AssetID, asset.Type)
	}
}

// waitNoPending holds off while a transaction from the user address is still in the mempool
// (an earlier attempt's Sweep, say): signing another now would stack a second one behind it
// on a balance the first is about to spend.
func (s *Sweeper) waitNoPending(ctx context.Context, user common.Address) error {
	deadline := time.Now().Add(s.Config.InclusionTimeout)
	for {
		pending, err := s.Chain.PendingNonce(ctx, user)
		if err != nil {
			return fmt.Errorf("reading pending nonce: %w", err)
		}
		latest, err := s.Chain.LatestNonce(ctx, user)
		if err != nil {
			return fmt.Errorf("reading nonce: %w", err)
		}
		if pending == latest {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("a transaction from %s has been pending for %s", user, s.Config.InclusionTimeout)
		}
		if err := sleep(ctx, s.Config.PollInterval); err != nil {
			return err
		}
	}
}

// sweepNative: gas is paid in the asset being moved, so nothing is topped up; wallet-service is
// asked for "max" and reserves the transaction's own fee itself (invariant #14, Open Question
// #11 decided as option (a)).
func (s *Sweeper) sweepNative(ctx context.Context, ev events.DepositConfirmed, user common.Address) error {
	bal, err := s.Chain.BalanceAt(ctx, user) // fresh read, invariant #10
	if err != nil {
		return fmt.Errorf("reading balance: %w", err)
	}
	if bal.Sign() == 0 {
		return nil // already swept
	}
	raw, err := s.Wallet.SignUserAddress(ctx, wallet.UserRequest{
		UserID: ev.UserID, ToAddress: s.Config.HotWallet.Hex(), AssetID: ev.AssetID, Amount: "max", ChainID: ev.ChainID,
	})
	switch {
	case errors.Is(err, wallet.ErrBelowGas):
		return nil // dust: not worth its own gas, and already credited in the ledger (invariant #14)
	case err != nil:
		return classify(err)
	}
	return s.sweepTx(ctx, raw)
}

func (s *Sweeper) sweepToken(ctx context.Context, ev events.DepositConfirmed, user common.Address, asset registry.Asset) error {
	tokens, err := s.Chain.TokenBalance(ctx, asset.Contract, user) // fresh read, invariant #10
	if err != nil {
		return fmt.Errorf("reading token balance: %w", err)
	}
	if tokens.Sign() == 0 {
		return nil // already swept
	}
	native, err := s.Chain.BalanceAt(ctx, user)
	if err != nil {
		return fmt.Errorf("reading native balance: %w", err)
	}
	// Gas Top-Up only when needed (invariant #8): a retry never re-funds an address that is funded.
	if native.Cmp(s.Config.GasReserve) < 0 {
		if err := s.topUp(ctx, ev, user); err != nil {
			return err
		}
		// Time has passed; the balance may have changed (a second deposit, a parallel sweep).
		if tokens, err = s.Chain.TokenBalance(ctx, asset.Contract, user); err != nil {
			return fmt.Errorf("reading token balance: %w", err)
		}
		if tokens.Sign() == 0 {
			return nil
		}
	}
	raw, err := s.Wallet.SignUserAddress(ctx, wallet.UserRequest{
		UserID: ev.UserID, ToAddress: s.Config.HotWallet.Hex(), AssetID: ev.AssetID, Amount: tokens.String(), ChainID: ev.ChainID,
	})
	if err != nil {
		return classify(err)
	}
	return s.sweepTx(ctx, raw)
}

// sweepTx sends a Sweep, waits for it, and reports its gas.
func (s *Sweeper) sweepTx(ctx context.Context, raw []byte) error {
	hash, rc, err := s.broadcastAndWait(ctx, raw, "sweep")
	if err != nil {
		return err
	}
	s.postGas(ctx, hash, rc, "sweep")
	return nil
}

func (s *Sweeper) topUp(ctx context.Context, ev events.DepositConfirmed, user common.Address) error {
	nativeID, ok := s.Assets.NativeID()
	if !ok {
		return fmt.Errorf("%w: no native asset in the registry to pay a Gas Top-Up with", ErrPermanent)
	}
	raw, nonce, err := s.Wallet.SignHotWallet(ctx, wallet.HotRequest{
		ToAddress: user.Hex(), AssetID: nativeID, Amount: s.Config.GasTopUp.String(), ChainID: ev.ChainID,
		IdempotencyKey: TopUpKey(ev.ChainID, ev.TxHash, ev.LogIndex),
	})
	if err != nil {
		return classify(err)
	}
	log.Printf("gas top-up for deposit %s (hot wallet nonce %d)", ev.Key(), nonce)
	key := TopUpKey(ev.ChainID, ev.TxHash, ev.LogIndex)
	hash, rc, err := s.sendHotTx(ctx, key, raw)
	if err != nil {
		return err
	}
	s.postGas(ctx, hash, rc, "gas_topup")
	return nil
}

// broadcastAndWait sends the signed transaction and waits until it is mined. Waiting matters:
// the next step (or the next retry) must read the balance after this one has landed, not
// while it is still in flight.
func (s *Sweeper) broadcastAndWait(ctx context.Context, raw []byte, what string) (common.Hash, *chain.Receipt, error) {
	hash, err := s.Chain.Broadcast(ctx, raw)
	if err != nil {
		return hash, nil, fmt.Errorf("broadcasting %s: %w", what, err)
	}
	deadline := time.Now().Add(s.Config.InclusionTimeout)
	for {
		r, err := s.Chain.Receipt(ctx, hash)
		if err != nil {
			return hash, nil, fmt.Errorf("reading %s receipt: %w", what, err)
		}
		if r != nil {
			if !r.Success {
				return hash, r, fmt.Errorf("%s %s reverted in block %d", what, hash, r.Block)
			}
			return hash, r, nil
		}
		if time.Now().After(deadline) {
			return hash, nil, fmt.Errorf("%s %s not mined within %s", what, hash, s.Config.InclusionTimeout)
		}
		if err := sleep(ctx, s.Config.PollInterval); err != nil {
			return hash, nil, err
		}
	}
}

func classify(err error) error {
	if errors.Is(err, wallet.ErrPermanent) {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
