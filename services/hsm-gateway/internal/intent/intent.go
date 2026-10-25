// Package intent is the Sign Intent: the unsigned EIP-1559 fields the gateway signs from
// (key-custody-hsm.md invariant 3). The wire schema deliberately has no digest, hash, signature or
// raw-transaction field, and unknown fields are rejected, so a caller cannot supply a digest.
package intent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Operation classes (policy.md section 4.1 invariant 4). Replacement is not a class the caller
// claims: it is detected by a nonce the gateway has already signed.
const (
	OpPayout        = "payout"         // withdrawal, Gas Top-Up: counts against the Policy Windows
	OpTreasurySweep = "treasury_sweep" // Hot -> the policy-pinned Warm address
	OpCancel        = "cancel"         // Nonce Filler / void: zero value, to Hot
)

// MaxNonce is reserved for the startup self-test transaction (EIP-2681: a nonce >= 2^64-1 can
// never be included), so no caller intent may use it.
const MaxNonce = ^uint64(0)

// Wire is the JSON body of POST /v1/hot/sign. Value and fees are decimal strings (wei), data is hex.
type Wire struct {
	ChainID              uint64 `json:"chain_id"`
	Nonce                uint64 `json:"nonce"`
	To                   string `json:"to"`
	Value                string `json:"value"`
	Data                 string `json:"data"`
	Gas                  uint64 `json:"gas"`
	MaxFeePerGas         string `json:"max_fee_per_gas"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas"`
	Operation            string `json:"operation"`
	IdempotencyKey       string `json:"idempotency_key"`
	// CallerCN is a claim only (audited next to the verified certificate CN, never trusted).
	CallerCN string `json:"caller_cn,omitempty"`
}

// Intent is a validated Wire.
type Intent struct {
	ChainID        uint64
	Nonce          uint64
	To             common.Address
	Value          *big.Int
	Data           []byte
	Gas            uint64
	MaxFee         *big.Int
	Tip            *big.Int
	Operation      string
	IdempotencyKey string
	CallerCNClaim  string
}

// ErrInvalid wraps every validation failure (HTTP 400, not a policy refusal).
var ErrInvalid = errors.New("invalid sign intent")

var max256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// Decode reads one Wire from r, rejecting unknown fields and trailing data.
func Decode(r io.Reader) (*Intent, error) {
	dec := json.NewDecoder(io.LimitReader(r, 64<<10))
	dec.DisallowUnknownFields()
	var w Wire
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	return Parse(w)
}

// Parse validates the wire fields.
func Parse(w Wire) (*Intent, error) {
	bad := func(f, why string) (*Intent, error) { return nil, fmt.Errorf("%w: %s %s", ErrInvalid, f, why) }
	if w.ChainID == 0 {
		return bad("chain_id", "is required")
	}
	if !isHexAddr(w.To) {
		return bad("to", "must be a 0x-prefixed 20-byte address")
	}
	val, ok := parseUint(w.Value)
	if !ok {
		return bad("value", "must be a decimal integer string")
	}
	maxFee, ok := parseUint(w.MaxFeePerGas)
	if !ok {
		return bad("max_fee_per_gas", "must be a decimal integer string")
	}
	tip, ok := parseUint(w.MaxPriorityFeePerGas)
	if !ok {
		return bad("max_priority_fee_per_gas", "must be a decimal integer string")
	}
	data, err := parseHex(w.Data)
	if err != nil {
		return bad("data", "must be 0x-prefixed hex")
	}
	if w.Gas == 0 {
		return bad("gas", "must be positive")
	}
	if len(w.IdempotencyKey) == 0 || len(w.IdempotencyKey) > 128 {
		return bad("idempotency_key", "is required (1-128 chars)")
	}
	if len(w.CallerCN) > 128 {
		return bad("caller_cn", "too long")
	}
	return &Intent{
		ChainID: w.ChainID, Nonce: w.Nonce, To: common.HexToAddress(w.To), Value: val, Data: data,
		Gas: w.Gas, MaxFee: maxFee, Tip: tip, Operation: w.Operation,
		IdempotencyKey: w.IdempotencyKey, CallerCNClaim: w.CallerCN,
	}, nil
}

// Tx rebuilds the unsigned EIP-1559 transaction: type 2, empty access list, a recipient always set.
func (in *Intent) Tx() *types.Transaction {
	to := in.To
	return types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(in.ChainID),
		Nonce:     in.Nonce,
		GasTipCap: new(big.Int).Set(in.Tip),
		GasFeeCap: new(big.Int).Set(in.MaxFee),
		Gas:       in.Gas,
		To:        &to,
		Value:     new(big.Int).Set(in.Value),
		Data:      append([]byte(nil), in.Data...),
	})
}

// Digest is the hash the HSM signs: computed here, from the intent, never taken from a caller.
func (in *Intent) Digest() [32]byte {
	return types.LatestSignerForChainID(new(big.Int).SetUint64(in.ChainID)).Hash(in.Tx())
}

// Seal attaches a canonical 65-byte [R||S||V] signature and returns the transaction and its raw bytes.
func (in *Intent) Seal(sig65 []byte) (*types.Transaction, []byte, error) {
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(in.ChainID))
	tx, err := in.Tx().WithSignature(signer, sig65)
	if err != nil {
		return nil, nil, err
	}
	raw, err := tx.MarshalBinary()
	return tx, raw, err
}

// SelfTestIntent is the fixed transaction the startup/periodic self-test signs: nonce 2^64-1 (can
// never be mined), zero value to the Hot address itself.
func SelfTestIntent(chainID uint64, hot common.Address) *Intent {
	return &Intent{
		ChainID: chainID, Nonce: MaxNonce, To: hot, Value: new(big.Int), Gas: 21000,
		MaxFee: big.NewInt(1), Tip: big.NewInt(1), Operation: "self_test", IdempotencyKey: "self-test",
	}
}

func isHexAddr(s string) bool {
	if !strings.HasPrefix(s, "0x") || len(s) != 42 {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

func parseHex(s string) ([]byte, error) {
	if s == "" || s == "0x" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "0x") {
		return nil, errors.New("no 0x prefix")
	}
	return hex.DecodeString(s[2:])
}

func parseUint(s string) (*big.Int, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return nil, false
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Cmp(max256) > 0 {
		return nil, false
	}
	return v, true
}

// SameBody reports whether a and b rebuild to the same transaction fields apart from fees.
func SameBody(a, b *Intent) bool {
	return a.To == b.To && a.Value.Cmp(b.Value) == 0 && bytes.Equal(a.Data, b.Data) && a.Gas == b.Gas
}
