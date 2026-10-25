package signing

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/wallet-service/internal/storage"
)

// Fee Bump (withdrawal-flow.md invariant #21), Signature Void (#22) and Nonce Filler (#23).

// ErrKeyVoided: the key was voided, so it is never signed (HTTP 410).
var ErrKeyVoided = storage.ErrKeyVoided

// bumpNumerator/bumpDenominator: a replacement pays at least 12.5% more on both fees (nodes demand 10%).
const bumpNumerator, bumpDenominator = 1125, 1000

// ReplaceRequest is POST .../replace's body. There is deliberately no field that could change the
// destination, amount or data: a replacement is rebuilt from the stored original.
type ReplaceRequest struct {
	ReplacementSeq int    `json:"replacement_seq"`
	MaxGasPrice    string `json:"max_gas_price,omitempty"`
}

type ReplaceResponse struct {
	SignedRawTx          string `json:"signed_raw_tx"`
	TxHash               string `json:"tx_hash"`
	Nonce                uint64 `json:"nonce"`
	ReplacementSeq       int    `json:"replacement_seq"`
	MaxFeePerGas         string `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas"`
}

// VoidResponse is POST .../void's answer: {"status":"voided"} or {"status":"already_signed",...}.
type VoidResponse struct {
	Status      string `json:"status"`
	Nonce       uint64 `json:"nonce,omitempty"`
	SignedRawTx string `json:"signed_raw_tx,omitempty"`
	TxHash      string `json:"tx_hash,omitempty"`
}

// KeyView is GET .../{key}'s answer.
type KeyView struct {
	Nonce        uint64            `json:"nonce"`
	SignedRawTx  string            `json:"signed_raw_tx"`
	TxHash       string            `json:"tx_hash"`
	Replacements []ReplacementView `json:"replacements"`
}

type ReplacementView struct {
	ReplacementSeq       int    `json:"replacement_seq"`
	Kind                 string `json:"kind"`
	SignedRawTx          string `json:"signed_raw_tx"`
	TxHash               string `json:"tx_hash"`
	MaxFeePerGas         string `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas"`
}

func hexTx(raw []byte) string { return "0x" + hex.EncodeToString(raw) }

func decodeTx(raw []byte) (*types.Transaction, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, fmt.Errorf("stored tx is undecodable: %w", err)
	}
	return &tx, nil
}

func txHash(raw []byte) (string, error) {
	tx, err := decodeTx(raw)
	if err != nil {
		return "", err
	}
	return strings.ToLower(tx.Hash().Hex()), nil
}

// Lookup is GET /v1/sign/hot-wallet/{key}: read-only, scoped to the caller's own keys
// (storage.ErrNotFound otherwise). Not enough to Release a request — that is what Void is for.
func (s *Signer) Lookup(ctx context.Context, caller, key string) (KeyView, error) {
	k, err := s.store.GetHotKey(ctx, caller, key)
	if err != nil {
		return KeyView{}, err
	}
	h, err := txHash(k.SignedRawTx)
	if err != nil {
		return KeyView{}, err
	}
	v := KeyView{Nonce: k.Nonce, SignedRawTx: hexTx(k.SignedRawTx), TxHash: h, Replacements: []ReplacementView{}}
	for _, r := range k.Replacements {
		v.Replacements = append(v.Replacements, replacementView(r))
	}
	return v, nil
}

func replacementView(r storage.Replacement) ReplacementView {
	return ReplacementView{ReplacementSeq: r.Seq, Kind: r.Kind, SignedRawTx: hexTx(r.SignedRawTx), TxHash: r.TxHash,
		MaxFeePerGas: r.MaxFeePerGas.String(), MaxPriorityFeePerGas: r.MaxTip.String()}
}

// Void is POST .../void: it either returns the signature the key already has, or promises —
// durably, under the lock signing takes — that the key is never signed.
func (s *Signer) Void(ctx context.Context, caller, key string) (VoidResponse, error) {
	if key == "" || len(key) > 128 {
		return VoidResponse{}, fmt.Errorf("%w: key is required (max 128 chars)", ErrInvalidRequest)
	}
	res, err := s.store.VoidHotKey(ctx, s.chainID, s.hotKey, caller, key,
		func(ctx context.Context) (uint64, error) { return s.chain.PendingNonceAt(ctx, s.from) })
	if err != nil {
		return VoidResponse{}, err
	}
	if res.Voided {
		return VoidResponse{Status: "voided"}, nil
	}
	h, err := txHash(res.Signed.SignedRawTx)
	if err != nil {
		return VoidResponse{}, err
	}
	return VoidResponse{Status: "already_signed", Nonce: res.Signed.Nonce, SignedRawTx: hexTx(res.Signed.SignedRawTx), TxHash: h}, nil
}

// Replace is POST .../replace: the same tx re-signed at the same nonce with higher fees (a Fee Bump).
func (s *Signer) Replace(ctx context.Context, caller, key string, req ReplaceRequest) (ReplaceResponse, error) {
	if req.ReplacementSeq < 1 {
		return ReplaceResponse{}, fmt.Errorf("%w: replacement_seq must be >= 1", ErrInvalidRequest)
	}
	var maxGas *big.Int
	if req.MaxGasPrice != "" {
		var ok bool
		if maxGas, ok = parseUint(req.MaxGasPrice); !ok || maxGas.Sign() == 0 {
			return ReplaceResponse{}, fmt.Errorf("%w: max_gas_price must be a positive decimal string (wei)", ErrInvalidRequest)
		}
	}
	r, err := s.replace(ctx, caller, key, req.ReplacementSeq, storage.KindFeeBump, maxGas)
	if err != nil {
		return ReplaceResponse{}, err
	}
	return ReplaceResponse{SignedRawTx: hexTx(r.SignedRawTx), TxHash: r.TxHash, Nonce: r.Nonce, ReplacementSeq: r.Seq,
		MaxFeePerGas: r.MaxFeePerGas.String(), MaxPriorityFeePerGas: r.MaxTip.String()}, nil
}

// Cancel is the operator's Nonce Filler (invariant #23): a 0-value Hot Wallet -> Hot Wallet tx at
// the key's nonce with higher fees, recorded as a `cancel` replacement. It consumes the nonce, so
// the withdrawal can never land, and next_nonce needs no reset. Not reachable over the network.
// Running it again returns the same filler (to be re-sent).
func (s *Signer) Cancel(ctx context.Context, key string) (storage.Replacement, error) {
	return s.replace(ctx, "", key, 0, storage.KindCancel, nil)
}

func (s *Signer) replace(ctx context.Context, caller, key string, seq int, kind string, maxGas *big.Int) (storage.Replacement, error) {
	// A known-down signer fails fast, before the network and before the DB lock.
	if err := s.hot.Available(); err != nil {
		return storage.Replacement{}, err
	}
	// The network is read before the lock is taken, like signing.
	netTip, baseFee, err := s.chain.FeeQuote(ctx)
	if err != nil {
		return storage.Replacement{}, fmt.Errorf("fetching fees: %w", err)
	}
	netCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), netTip)
	limit := s.feeCeiling
	if maxGas != nil && maxGas.Cmp(limit) < 0 {
		limit = maxGas
	}

	return s.store.ReplaceHotKey(ctx, s.chainID, s.hotKey, caller, key, seq, kind, func(in storage.ReplaceInput) (storage.Built, error) {
		orig, err := decodeTx(in.OriginalRaw)
		if err != nil {
			return storage.Built{}, err
		}
		prev, err := decodeTx(in.LatestRaw)
		if err != nil {
			return storage.Built{}, err
		}
		tip, feeCap := bumpedFees(prev.GasTipCap(), prev.GasFeeCap(), netTip, netCap)
		if feeCap.Cmp(limit) > 0 {
			return storage.Built{}, fmt.Errorf("%w: a replacement needs max fee %s wei, above the ceiling %s", ErrGasCeilingExceeded, feeCap, limit)
		}
		body := txBody{to: *orig.To(), value: orig.Value(), data: orig.Data(), gas: orig.Gas(), tip: tip, cap_: feeCap}
		// A Fee Bump is signed under the class of the payout it replaces; a Nonce Filler is a cancel.
		meta := HotMeta{Operation: OpPayout, IdempotencyKey: key, Caller: caller}
		if kind == storage.KindCancel {
			body = txBody{to: s.from, value: new(big.Int), gas: 21000, tip: tip, cap_: feeCap}
			meta.Operation = OpCancel
		}
		raw, err := s.signHot(ctx, meta, in.Nonce, body)
		if err != nil {
			return storage.Built{}, err
		}
		h, err := txHash(raw)
		if err != nil {
			return storage.Built{}, err
		}
		return storage.Built{Raw: raw, Hash: h, MaxFee: feeCap, MaxTip: tip}, nil
	})
}

// bumpedFees is max(current network fee, previous version's fee × 1.125, rounded up) on both
// fields; the cap never falls below the tip.
func bumpedFees(prevTip, prevCap, netTip, netCap *big.Int) (tip, feeCap *big.Int) {
	tip = bigMax(netTip, ceilBump(prevTip))
	feeCap = bigMax(netCap, ceilBump(prevCap))
	if feeCap.Cmp(tip) < 0 {
		feeCap = new(big.Int).Set(tip)
	}
	return tip, feeCap
}

func ceilBump(x *big.Int) *big.Int {
	n := new(big.Int).Mul(x, big.NewInt(bumpNumerator))
	n.Add(n, big.NewInt(bumpDenominator-1))
	return n.Div(n, big.NewInt(bumpDenominator))
}

func bigMax(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return new(big.Int).Set(a)
	}
	return new(big.Int).Set(b)
}
