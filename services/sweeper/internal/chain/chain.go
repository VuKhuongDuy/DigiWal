// Package chain is sweeper's view of an EVM node: balances, nonces, broadcast and receipts.
// sweeper never holds a key; it only reads the chain and broadcasts what wallet-service signed.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

type Receipt struct {
	Success bool
	Block   int64
	// GasUsed and EffectiveGasPrice (wei) are what the tx cost: the ledger's Gas Fee is their product.
	GasUsed           uint64
	EffectiveGasPrice *big.Int
}

type Chain interface {
	ChainID(ctx context.Context) (int64, error)
	// BalanceAt is the native balance at the latest block.
	BalanceAt(ctx context.Context, addr common.Address) (*big.Int, error)
	// TokenBalance is balanceOf(holder) on an ERC-20 contract, at the latest block.
	TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error)
	// PendingNonce and LatestNonce differ exactly when a transaction from addr sits in the mempool.
	PendingNonce(ctx context.Context, addr common.Address) (uint64, error)
	LatestNonce(ctx context.Context, addr common.Address) (uint64, error)
	// Broadcast sends a signed raw transaction. A node that already knows the transaction is
	// not an error: broadcasting the same signed bytes again must be harmless.
	Broadcast(ctx context.Context, rawTx []byte) (common.Hash, error)
	// Receipt returns nil while the transaction is not mined.
	Receipt(ctx context.Context, tx common.Hash) (*Receipt, error)
	// Head is the latest block number: a stuck Gas Top-Up is measured in blocks, not minutes.
	Head(ctx context.Context) (int64, error)
	// Underpriced reports whether the signed tx's max fee is below the latest block's base fee, i.e.
	// it cannot be mined at this price (a Fee Bump's cue).
	Underpriced(ctx context.Context, rawTx []byte) (bool, error)
}

// Client is the slice of go-ethereum's client Eth uses (satisfied by *ethclient.Client and by
// the simulated backend the tests run on).
type Client interface {
	ChainID(ctx context.Context) (*big.Int, error)
	BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

type Eth struct{ c Client }

func Dial(ctx context.Context, rpcURL string) (*Eth, error) {
	c, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", rpcURL, err)
	}
	return &Eth{c: c}, nil
}

func NewEth(c Client) *Eth { return &Eth{c: c} }

func (e *Eth) ChainID(ctx context.Context) (int64, error) {
	id, err := e.c.ChainID(ctx)
	if err != nil {
		return 0, err
	}
	return id.Int64(), nil
}

func (e *Eth) BalanceAt(ctx context.Context, addr common.Address) (*big.Int, error) {
	return e.c.BalanceAt(ctx, addr, nil)
}

// balanceOf(address) selector.
var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31}

func (e *Eth) TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(holder.Bytes(), 32)...)
	out, err := e.c.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("balanceOf on %s: %w", token, err)
	}
	if len(out) != 32 { // no code at the address, or not an ERC-20: never treat that as a balance
		return nil, fmt.Errorf("balanceOf on %s returned %d bytes, want 32", token, len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

func (e *Eth) PendingNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return e.c.PendingNonceAt(ctx, addr)
}

func (e *Eth) LatestNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return e.c.NonceAt(ctx, addr, nil)
}

func (e *Eth) Broadcast(ctx context.Context, rawTx []byte) (common.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return common.Hash{}, fmt.Errorf("decoding signed transaction: %w", err)
	}
	err := e.c.SendTransaction(ctx, &tx)
	if err != nil && !alreadyKnown(err) {
		return common.Hash{}, err
	}
	return tx.Hash(), nil
}

// alreadyKnown recognizes a node saying it has this exact transaction (pending or mined).
// The strings differ between clients; the caller still verifies by waiting for the receipt.
func alreadyKnown(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already known") || strings.Contains(m, "known transaction") || strings.Contains(m, "already exists")
}

func (e *Eth) Receipt(ctx context.Context, tx common.Hash) (*Receipt, error) {
	r, err := e.c.TransactionReceipt(ctx, tx)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	price := r.EffectiveGasPrice
	if price == nil {
		price = new(big.Int)
	}
	return &Receipt{Success: r.Status == types.ReceiptStatusSuccessful, Block: r.BlockNumber.Int64(), GasUsed: r.GasUsed, EffectiveGasPrice: price}, nil
}

func (e *Eth) Head(ctx context.Context) (int64, error) {
	n, err := e.c.BlockNumber(ctx)
	return int64(n), err
}

func (e *Eth) Underpriced(ctx context.Context, rawTx []byte) (bool, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return false, fmt.Errorf("decoding signed transaction: %w", err)
	}
	h, err := e.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return false, err
	}
	if h.BaseFee == nil {
		return false, nil
	}
	return tx.GasFeeCap().Cmp(h.BaseFee) < 0, nil
}
