// Package sweep is sweep-signer's one operation: sign a Sweep (a user's derived key -> the Hot Wallet) and nothing else
// (key-custody-hsm.md invariant 15). It is the code that used to be UserSigner in wallet-service, moved, not redesigned:
// destination = Hot (here: pinned from hsm-gateway, never from the request), live balance for "max", the fee CAP reserved,
// the live pending nonce. New in this home: the Fee Ceiling, the stored-address check (invariant 11), the audit gate and a
// counted, alerted refusal for every rule.
package sweep

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/sweep-signer/internal/assets"
	"digiwal/services/sweep-signer/internal/derive"
	"digiwal/services/sweep-signer/internal/store"
	"digiwal/services/sweep-signer/internal/vault"
)

// AmountMax: "everything but gas" (native Sweeps only).
const AmountMax = "max"

// Errors the HTTP layer maps to status codes.
var (
	ErrInvalidRequest   = errors.New("invalid request")
	ErrUnknownAsset     = errors.New("unknown asset")
	ErrUnsupportedChain = errors.New("unsupported chain_id")
	// ErrDestinationNotHotWallet: the Sweep's destination must be the Hot Wallet.
	ErrDestinationNotHotWallet = errors.New("destination must be the hot wallet")
	ErrUnknownUser             = errors.New("unknown user")
	// ErrBalanceBelowGas: a native "max" Sweep has nothing left after reserving gas.
	ErrBalanceBelowGas = errors.New("balance does not cover the sweep's gas")
	// ErrFeeCeiling: the live fee cap is above the Fee Ceiling. Transient: fees come down, the caller retries.
	ErrFeeCeiling = errors.New("current max fee per gas exceeds the Fee Ceiling")
	// ErrAddressMismatch: the key derived at the user's index is not the stored address (a swapped xpub, or a wrong Seed).
	ErrAddressMismatch = errors.New("the derived address does not match the issued address")
	// ErrFrozen: the Address Audit has not passed yet, or found a mismatch: no signing at all.
	ErrFrozen = errors.New("signing is frozen by the address audit")
	// ErrNotASweep: the transaction about to be signed is not a native transfer or an ERC-20 transfer to the Hot Wallet.
	ErrNotASweep = errors.New("the transaction is not a plain sweep to the hot wallet")
)

// Rule ids of the refusal counter (key-custody-hsm.md invariant 15).
const (
	RuleInvalid  = "invalid_request"
	RuleChain    = "chain"
	RuleAsset    = "asset"
	RuleDest     = "destination"
	RuleIndex    = "index"
	RuleFee      = "fee_ceiling"
	RuleMismatch = "address_mismatch"
	RuleFrozen   = "audit_frozen"
	RuleShape    = "tx_shape"
)

// DefaultFeeCeiling is 50 gwei, the Fee Ceiling decided for Hot Wallet replacements (withdrawal-flow.md Open Questions #18).
var DefaultFeeCeiling = big.NewInt(50_000_000_000)

// Request is POST /v1/sign/user-address's body, the contract sweeper has always used.
type Request struct {
	UserID    string `json:"user_id"`
	ToAddress string `json:"to_address"`
	AssetID   string `json:"asset_id"`
	Amount    string `json:"amount"`
	ChainID   int64  `json:"chain_id"`
}

// Response is the 200 body.
type Response struct {
	SignedRawTx string `json:"signed_raw_tx"`
}

// Chain is the slice of an EVM RPC client the signer needs.
type Chain interface {
	ChainID(ctx context.Context) (*big.Int, error)
	PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error)
	BalanceAt(ctx context.Context, addr common.Address) (*big.Int, error)
	FeeQuote(ctx context.Context) (tip, baseFee *big.Int, err error)
	EstimateGas(ctx context.Context, from common.Address, to common.Address, value *big.Int, data []byte) (uint64, error)
}

// Directory resolves a user to the address row issued for them.
type Directory interface {
	Issued(ctx context.Context, userID, chain string) (store.Issued, error)
}

// Seeds hands the Seed to fn for one call (vault.Provider).
type Seeds interface {
	With(ctx context.Context, fn func(seed []byte) error) error
}

// Gate is the Address Audit's verdict: nil means signing may proceed. Trip freezes it for good (until restart).
type Gate interface {
	Ready() error
	Trip(reason string)
}

// Observer counts refusals.
type Observer interface{ Refused(rule string) }

type noObserver struct{}

func (noObserver) Refused(string) {}

// Signer signs Sweeps.
type Signer struct {
	dir     Directory
	seeds   Seeds
	chain   Chain
	assets  assets.Assets
	gate    Gate
	obs     Observer
	chainID int64
	hot     common.Address
	ceiling *big.Int
}

// New pins the chain and the Hot address. chainID is the PINNED chain (from hsm-gateway); the RPC must agree with it.
func New(ctx context.Context, dir Directory, seeds Seeds, chain Chain, a assets.Assets, hot common.Address, pinnedChain uint64, gate Gate) (*Signer, error) {
	id, err := chain.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading chain id from RPC: %w", err)
	}
	if id.Uint64() != pinnedChain {
		return nil, fmt.Errorf("the pinned chain is %d but the RPC is chain %d", pinnedChain, id)
	}
	return &Signer{dir: dir, seeds: seeds, chain: chain, assets: a, hot: hot, chainID: id.Int64(), gate: gate,
		obs: noObserver{}, ceiling: DefaultFeeCeiling}, nil
}

// WithObserver sets the refusal counter.
func (s *Signer) WithObserver(o Observer) *Signer { s.obs = o; return s }

// WithFeeCeiling sets the Fee Ceiling (max fee per gas, wei).
func (s *Signer) WithFeeCeiling(wei *big.Int) *Signer { s.ceiling = new(big.Int).Set(wei); return s }

// Hot is the pinned Hot address; ChainID the pinned chain.
func (s *Signer) Hot() common.Address { return s.hot }
func (s *Signer) ChainID() int64      { return s.chainID }

// refuse counts the refusal, raises the alert for the invariant-15 rules and returns err.
func (s *Signer) refuse(rule string, err error) error {
	s.obs.Refused(rule)
	if rule != RuleInvalid {
		log.Printf("ALERT sweep refused: rule=%s: %v", rule, err)
	}
	return err
}

// Sign builds and signs one Sweep transaction. The destination is checked against the pinned Hot Wallet, the caller's
// to_address is never trusted. The nonce is the user address's live pending nonce (a user address has no concurrent second
// signer, so no DB counter). No idempotency key: the caller always signs the live balance, so a retry is harmless.
func (s *Signer) Sign(ctx context.Context, req Request) (Response, error) {
	if err := s.gate.Ready(); err != nil {
		return Response{}, s.refuse(RuleFrozen, fmt.Errorf("%w: %v", ErrFrozen, err))
	}
	bad := func(msg string) (Response, error) {
		return Response{}, s.refuse(RuleInvalid, fmt.Errorf("%w: %s", ErrInvalidRequest, msg))
	}
	if req.UserID == "" {
		return bad("user_id is required")
	}
	if !common.IsHexAddress(req.ToAddress) {
		return bad("to_address must be a 20-byte hex address")
	}
	sweepMax := req.Amount == AmountMax
	var amount *big.Int
	if !sweepMax {
		var ok bool
		if amount, ok = parseUint(req.Amount); !ok || amount.Sign() == 0 {
			return bad(`amount must be a positive decimal string, or "max" for a native-coin sweep`)
		}
	}
	if req.ChainID != s.chainID {
		return Response{}, s.refuse(RuleChain, ErrUnsupportedChain)
	}
	asset, ok := s.assets.Lookup(req.AssetID)
	if !ok {
		return Response{}, s.refuse(RuleAsset, ErrUnknownAsset)
	}
	if common.HexToAddress(req.ToAddress) != s.hot {
		return Response{}, s.refuse(RuleDest, ErrDestinationNotHotWallet)
	}

	row, err := s.dir.Issued(ctx, req.UserID, store.ChainEVM)
	if errors.Is(err, store.ErrNotFound) {
		return Response{}, s.refuse(RuleIndex, ErrUnknownUser)
	}
	if err != nil {
		return Response{}, err
	}
	if !common.IsHexAddress(row.Address) {
		return Response{}, s.refuse(RuleIndex, fmt.Errorf("the issued address row of user %q is malformed", req.UserID))
	}
	from := common.HexToAddress(row.Address)

	if sweepMax {
		if asset.Type != assets.Native {
			// ERC-20 gas is paid separately (Gas Top-Up), so its amount is the full token balance, which only the caller knows.
			return bad(`"max" is only valid for a native-coin sweep`)
		}
		bal, err := s.chain.BalanceAt(ctx, from)
		if err != nil {
			return Response{}, fmt.Errorf("reading balance: %w", err)
		}
		amount = bal
	}
	body, err := buildBody(ctx, s.chain, from, s.hot, amount, asset)
	if err != nil {
		return Response{}, err
	}
	if body.cap_.Cmp(s.ceiling) > 0 {
		return Response{}, s.refuse(RuleFee, fmt.Errorf("%w: %s > %s", ErrFeeCeiling, body.cap_, s.ceiling))
	}
	if sweepMax {
		// A node only accepts a tx if balance >= value + gas*maxFeePerGas, so reserve the fee *cap*, not the (lower) expected
		// cost; the difference stays as dust.
		fee := new(big.Int).Mul(new(big.Int).SetUint64(body.gas), body.cap_)
		sweepable := new(big.Int).Sub(amount, fee)
		if sweepable.Sign() <= 0 {
			return Response{}, ErrBalanceBelowGas
		}
		body.value = sweepable
	}
	nonce, err := s.chain.PendingNonceAt(ctx, from)
	if err != nil {
		return Response{}, fmt.Errorf("reading nonce: %w", err)
	}
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: big.NewInt(s.chainID), Nonce: nonce, GasTipCap: body.tip, GasFeeCap: body.cap_,
		Gas: body.gas, To: &body.to, Value: body.value, Data: body.data,
	})
	if err := s.checkShape(unsigned, asset); err != nil {
		return Response{}, s.refuse(RuleShape, err)
	}

	var signed *types.Transaction
	err = s.seeds.With(ctx, func(seed []byte) error {
		var e error
		signed, e = derive.SignTx(seed, row.Index, from, unsigned, big.NewInt(s.chainID))
		return e
	})
	switch {
	case errors.Is(err, derive.ErrWrongKey):
		// The stored address is not what the Seed derives at its index: the xpub (or the Seed) is not the one that issued
		// it. Refuse this Sweep and everything after it until an operator looks (invariant 11).
		s.gate.Trip(fmt.Sprintf("user %q index %d: derived key is not the issued address", req.UserID, row.Index))
		return Response{}, s.refuse(RuleMismatch, ErrAddressMismatch)
	case err != nil:
		return Response{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return Response{}, err
	}
	return Response{SignedRawTx: "0x" + hex.EncodeToString(raw)}, nil
}

// checkShape is the last gate before a key is touched: only a native transfer to the Hot Wallet, or an ERC-20
// transfer(hot, amount) on the registered contract, is ever signed (invariant 15: "no other operation").
func (s *Signer) checkShape(tx *types.Transaction, a assets.Asset) error {
	if tx.To() == nil {
		return fmt.Errorf("%w: contract creation", ErrNotASweep)
	}
	switch a.Type {
	case assets.Native:
		if *tx.To() != s.hot || len(tx.Data()) != 0 || tx.Value().Sign() <= 0 {
			return fmt.Errorf("%w: native", ErrNotASweep)
		}
	case assets.ERC20:
		d := tx.Data()
		if *tx.To() != a.Contract || tx.Value().Sign() != 0 || len(d) != 68 ||
			!bytes.Equal(d[:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) || common.BytesToAddress(d[4:36]) != s.hot {
			return fmt.Errorf("%w: erc20", ErrNotASweep)
		}
	default:
		return fmt.Errorf("%w: asset type %q", ErrNotASweep, a.Type)
	}
	return nil
}

func parseUint(s string) (*big.Int, bool) {
	if s == "" || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok
}

// erc20TransferData is ABI-encoded transfer(address,uint256): selector a9059cbb.
func erc20TransferData(to common.Address, amount *big.Int) []byte {
	data := []byte{0xa9, 0x05, 0x9c, 0xbb}
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	return append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
}

// txBody is everything about a transaction except its nonce.
type txBody struct {
	to        common.Address
	value     *big.Int
	data      []byte
	gas       uint64
	tip, cap_ *big.Int
}

// buildBody turns "send amount of asset to `to`" into a fully priced body using the chain's live fees.
func buildBody(ctx context.Context, chain Chain, from, to common.Address, amount *big.Int, asset assets.Asset) (txBody, error) {
	b := txBody{to: to, value: amount}
	if asset.Type == assets.ERC20 {
		b.to, b.value, b.data = asset.Contract, new(big.Int), erc20TransferData(to, amount)
	}
	tip, baseFee, err := chain.FeeQuote(ctx)
	if err != nil {
		return b, fmt.Errorf("fetching fees: %w", err)
	}
	b.tip = tip
	b.cap_ = new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip) // headroom for 6 full blocks of base-fee growth
	if b.gas, err = chain.EstimateGas(ctx, from, b.to, b.value, b.data); err != nil {
		return b, fmt.Errorf("estimating gas: %w", err)
	}
	if asset.Type == assets.ERC20 {
		b.gas = b.gas * 12 / 10
	}
	return b, nil
}

// Provider is compile-time proof that vault.Provider is the Seeds the service wires.
var _ Seeds = (*vault.Provider)(nil)
