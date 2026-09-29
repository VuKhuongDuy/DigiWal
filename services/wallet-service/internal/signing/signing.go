// Package signing builds and signs Hot Wallet transactions
// (POST /v1/sign/hot-wallet). wallet-service owns full transaction
// construction — gas price and limit come from its own EVM RPC client, never
// from the caller (Tech Lead decision, integration-contracts.md Open Question
// #11); a caller may only supply an optional max_gas_price safety ceiling.
package signing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
)

// Errors the HTTP layer maps to status codes.
var (
	ErrInvalidRequest     = errors.New("invalid request")
	ErrUnknownAsset       = errors.New("unknown asset")
	ErrUnsupportedChain   = errors.New("unsupported chain_id")
	ErrGasCeilingExceeded = errors.New("current gas price exceeds max_gas_price")
	ErrNotConfigured      = errors.New("hot wallet signing not configured")
)

// Request is POST /v1/sign/hot-wallet's body. Amount and MaxGasPrice are decimal
// strings (minor units / wei) — never JSON numbers (integration-contracts.md).
type Request struct {
	ToAddress      string `json:"to_address"`
	AssetID        string `json:"asset_id"`
	Amount         string `json:"amount"`
	ChainID        int64  `json:"chain_id"`
	IdempotencyKey string `json:"idempotency_key"`
	MaxGasPrice    string `json:"max_gas_price,omitempty"`
}

type Response struct {
	SignedRawTx string `json:"signed_raw_tx"` // 0x-hex
	Nonce       uint64 `json:"nonce"`
}

// Chain is the slice of an EVM RPC client the signer needs.
type Chain interface {
	ChainID(ctx context.Context) (*big.Int, error)
	PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error)
	// FeeQuote returns the current EIP-1559 tip and base fee.
	FeeQuote(ctx context.Context) (tip, baseFee *big.Int, err error)
	EstimateGas(ctx context.Context, from common.Address, to common.Address, value *big.Int, data []byte) (uint64, error)
}

type Store interface {
	LookupHotSignature(ctx context.Context, key, requestHash string) (storage.HotSignature, error)
	SignHotWallet(ctx context.Context, chainID int64, key, requestHash string,
		seedNonce func(context.Context) (uint64, error), sign func(nonce uint64) ([]byte, error)) (storage.HotSignature, error)
}

type Signer struct {
	store   Store
	wallet  *hdwallet.Wallet
	chain   Chain
	assets  Assets
	chainID int64
	from    common.Address
}

// New checks that the configured RPC really is the chain we were told it is, and
// pins the Hot Wallet address. Only that one chain_id is ever signed for.
func New(ctx context.Context, store Store, hot *hdwallet.Wallet, chain Chain, assets Assets) (*Signer, error) {
	id, err := chain.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading chain id from RPC: %w", err)
	}
	addr, err := hot.AddressAt(hdwallet.HotWalletIndex)
	if err != nil {
		return nil, err
	}
	return &Signer{store: store, wallet: hot, chain: chain, assets: assets, chainID: id.Int64(), from: common.HexToAddress(addr)}, nil
}

func (s *Signer) HotWalletAddress() common.Address { return s.from }

func (s *Signer) Sign(ctx context.Context, req Request) (Response, error) {
	to, amount, maxGas, asset, err := s.validate(req)
	if err != nil {
		return Response{}, err
	}
	hash := requestHash(req)

	// Fast path: a retried key replays the stored signature without any RPC.
	if sig, err := s.store.LookupHotSignature(ctx, req.IdempotencyKey, hash); err == nil {
		return toResponse(sig), nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Response{}, err
	}

	// Everything that needs the network happens before we touch the nonce.
	body, err := buildBody(ctx, s.chain, s.from, to, amount, asset, maxGas)
	if err != nil {
		return Response{}, err
	}

	sig, err := s.store.SignHotWallet(ctx, s.chainID, req.IdempotencyKey, hash,
		func(ctx context.Context) (uint64, error) { return s.chain.PendingNonceAt(ctx, s.from) },
		func(nonce uint64) ([]byte, error) {
			return signTx(s.wallet, hdwallet.HotWalletIndex, s.chainID, nonce, body)
		})
	if err != nil {
		return Response{}, err
	}
	return toResponse(sig), nil
}

func (s *Signer) validate(req Request) (to common.Address, amount, maxGas *big.Int, asset Asset, err error) {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, msg) }
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 128 {
		return to, nil, nil, asset, bad("idempotency_key is required (max 128 chars)")
	}
	if !common.IsHexAddress(req.ToAddress) {
		return to, nil, nil, asset, bad("to_address must be a 20-byte hex address")
	}
	to = common.HexToAddress(req.ToAddress)
	amount, ok := parseUint(req.Amount)
	if !ok || amount.Sign() == 0 {
		return to, nil, nil, asset, bad("amount must be a positive decimal string")
	}
	if req.MaxGasPrice != "" {
		if maxGas, ok = parseUint(req.MaxGasPrice); !ok || maxGas.Sign() == 0 {
			return to, nil, nil, asset, bad("max_gas_price must be a positive decimal string (wei)")
		}
	}
	if req.ChainID != s.chainID {
		return to, nil, nil, asset, ErrUnsupportedChain
	}
	asset, ok = s.assets.Lookup(req.AssetID)
	if !ok {
		return to, nil, nil, asset, ErrUnknownAsset
	}
	return to, amount, maxGas, asset, nil
}

func parseUint(s string) (*big.Int, bool) {
	if s == "" || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok
}

// requestHash binds an idempotency key to the exact request it was first used for.
func requestHash(r Request) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(r.ToAddress), r.AssetID, r.Amount, fmt.Sprint(r.ChainID), r.MaxGasPrice,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func toResponse(sig storage.HotSignature) Response {
	return Response{SignedRawTx: "0x" + hex.EncodeToString(sig.SignedRawTx), Nonce: sig.Nonce}
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

// buildBody turns business intent (send amount of asset to `to`) into a fully
// priced transaction body using the chain's live fees. maxGas, if set, is a
// ceiling: current fees above it refuse the request rather than sign an
// underpriced tx (a stuck tx would block every later nonce).
func buildBody(ctx context.Context, chain Chain, from, to common.Address, amount *big.Int, asset Asset, maxGas *big.Int) (txBody, error) {
	b := txBody{to: to, value: amount}
	if asset.Type == AssetERC20 {
		b.to, b.value, b.data = asset.Contract, new(big.Int), erc20TransferData(to, amount)
	}
	tip, baseFee, err := chain.FeeQuote(ctx)
	if err != nil {
		return b, fmt.Errorf("fetching fees: %w", err)
	}
	b.tip = tip
	b.cap_ = new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip) // headroom for 6 full blocks of base-fee growth
	if maxGas != nil && b.cap_.Cmp(maxGas) > 0 {
		return b, ErrGasCeilingExceeded
	}
	if b.gas, err = chain.EstimateGas(ctx, from, b.to, b.value, b.data); err != nil {
		return b, fmt.Errorf("estimating gas: %w", err)
	}
	if asset.Type == AssetERC20 {
		b.gas = b.gas * 12 / 10
	}
	return b, nil
}

func signTx(w *hdwallet.Wallet, index uint32, chainID int64, nonce uint64, b txBody) ([]byte, error) {
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: big.NewInt(chainID), Nonce: nonce, GasTipCap: b.tip, GasFeeCap: b.cap_,
		Gas: b.gas, To: &b.to, Value: b.value, Data: b.data,
	})
	signed, err := w.SignTxAt(index, unsigned, big.NewInt(chainID))
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	return signed.MarshalBinary()
}
