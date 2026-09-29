package signing

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
)

var (
	// ErrDestinationNotHotWallet: the Sweep's destination must be the Hot Wallet.
	ErrDestinationNotHotWallet = errors.New("destination must be the hot wallet")
	ErrUnknownUser             = errors.New("unknown user")
	// ErrBalanceBelowGas: a native "max" Sweep has nothing left after reserving gas.
	ErrBalanceBelowGas = errors.New("balance does not cover the sweep's gas")
)

// UserRequest is POST /v1/sign/user-address's body (the Sweep step).
//
// Amount is a decimal string in minor units, or — for a native-coin Sweep only —
// the literal "max": sweep everything except what the transaction's own gas needs
// (deposit-flow.md invariant #14, "Sweepable Amount").
type UserRequest struct {
	UserID    string `json:"user_id"`
	ToAddress string `json:"to_address"`
	AssetID   string `json:"asset_id"`
	Amount    string `json:"amount"`
	ChainID   int64  `json:"chain_id"`
}

type UserResponse struct {
	SignedRawTx string `json:"signed_raw_tx"`
}

// UserKeys resolves which derivation index belongs to a user.
type UserKeys interface {
	DerivationIndexFor(ctx context.Context, userID, chain string) (uint32, error)
}

// UserSigner signs Sweeps: from a user's own derived key, to the Hot Wallet,
// and nowhere else (deposit-flow.md invariants #6, #7).
type UserSigner struct {
	keys    UserKeys
	wallet  *hdwallet.Wallet // the user_derivation seed
	chain   Chain
	assets  Assets
	chainID int64
	hot     common.Address
}

func NewUserSigner(ctx context.Context, keys UserKeys, users *hdwallet.Wallet, chain Chain, assets Assets, hot common.Address) (*UserSigner, error) {
	id, err := chain.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading chain id from RPC: %w", err)
	}
	return &UserSigner{keys: keys, wallet: users, chain: chain, assets: assets, chainID: id.Int64(), hot: hot}, nil
}

// Sign builds and signs one Sweep transaction. The destination is checked
// server-side against the Hot Wallet — the caller's to_address is never
// trusted (invariant #7). The nonce is the user address's live pending nonce
// (invariant #12, PROPOSED): a user address has no concurrent second signer, so
// no DB counter is kept. No idempotency key is needed — the caller always signs
// the live balance, so a retry is harmless by construction (invariants #8, #10).
func (s *UserSigner) Sign(ctx context.Context, req UserRequest) (UserResponse, error) {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, msg) }
	if req.UserID == "" {
		return UserResponse{}, bad("user_id is required")
	}
	if !common.IsHexAddress(req.ToAddress) {
		return UserResponse{}, bad("to_address must be a 20-byte hex address")
	}
	sweepMax := req.Amount == AmountMax
	var amount *big.Int
	if !sweepMax {
		var ok bool
		if amount, ok = parseUint(req.Amount); !ok || amount.Sign() == 0 {
			return UserResponse{}, bad(`amount must be a positive decimal string, or "max" for a native-coin sweep`)
		}
	}
	if req.ChainID != s.chainID {
		return UserResponse{}, ErrUnsupportedChain
	}
	asset, ok := s.assets.Lookup(req.AssetID)
	if !ok {
		return UserResponse{}, ErrUnknownAsset
	}
	if common.HexToAddress(req.ToAddress) != s.hot {
		return UserResponse{}, ErrDestinationNotHotWallet
	}

	idx, err := s.keys.DerivationIndexFor(ctx, req.UserID, "evm")
	if errors.Is(err, storage.ErrNotFound) {
		return UserResponse{}, ErrUnknownUser
	}
	if err != nil {
		return UserResponse{}, err
	}
	addrHex, err := s.wallet.AddressAt(idx)
	if err != nil {
		return UserResponse{}, err
	}
	from := common.HexToAddress(addrHex)

	if sweepMax {
		if asset.Type != AssetNative {
			// ERC-20 gas is paid separately (Gas Top-Up), so its amount is the full
			// token balance, which only the caller knows.
			return UserResponse{}, bad(`"max" is only valid for a native-coin sweep`)
		}
		bal, err := s.chain.BalanceAt(ctx, from)
		if err != nil {
			return UserResponse{}, fmt.Errorf("reading balance: %w", err)
		}
		amount = bal
	}
	body, err := buildBody(ctx, s.chain, from, s.hot, amount, asset, nil)
	if err != nil {
		return UserResponse{}, err
	}
	if sweepMax {
		// A node only accepts a tx if balance >= value + gas*maxFeePerGas, so reserve
		// the fee *cap*, not the (lower) expected cost; the difference stays as dust.
		fee := new(big.Int).Mul(new(big.Int).SetUint64(body.gas), body.cap_)
		sweepable := new(big.Int).Sub(amount, fee)
		if sweepable.Sign() <= 0 {
			return UserResponse{}, ErrBalanceBelowGas
		}
		body.value = sweepable
	}
	nonce, err := s.chain.PendingNonceAt(ctx, from)
	if err != nil {
		return UserResponse{}, fmt.Errorf("reading nonce: %w", err)
	}
	raw, err := signTx(s.wallet, idx, s.chainID, nonce, body)
	if err != nil {
		return UserResponse{}, err
	}
	return UserResponse{SignedRawTx: "0x" + hex.EncodeToString(raw)}, nil
}
