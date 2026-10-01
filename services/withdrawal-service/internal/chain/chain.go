// Package chain is withdrawal-service's view of the EVM RPC: broadcast a signed tx, and read its
// receipt and the head to count confirmations.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

type Receipt struct {
	BlockNumber int64
	Success     bool
}

type Chain interface {
	ChainID(ctx context.Context) (int64, error)
	Head(ctx context.Context) (int64, error)
	// Broadcast sends the raw tx. A node that already has it is not an error.
	Broadcast(ctx context.Context, rawTx string) error
	// Receipt is nil (no error) while the tx is not in a block.
	Receipt(ctx context.Context, hash common.Hash) (*Receipt, error)
	// RevertReason replays a reverted tx (eth_call against the state before its block) and
	// returns the node's revert message. Best effort: "" when it cannot be recovered.
	RevertReason(ctx context.Context, hash common.Hash, block int64) (string, error)
	// MinedNonce is the account's nonce in the latest block: how many of its txs are mined. Never
	// the `pending` nonce, which counts our own tx still in the mempool.
	MinedNonce(ctx context.Context, addr common.Address) (uint64, error)
	// BaseFee is the latest block's base fee per gas (0 before London): a tx whose max fee is
	// below it cannot be mined (the Reaper's "underpriced" diagnosis).
	BaseFee(ctx context.Context) (*big.Int, error)
	// Balance is the account's native balance in the latest block.
	Balance(ctx context.Context, addr common.Address) (*big.Int, error)
}

// Sender recovers who signed a raw tx (for a withdrawal: the Hot Wallet).
func Sender(rawTx string) (common.Address, error) {
	b, err := hexutil.Decode(rawTx)
	if err != nil {
		return common.Address{}, err
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		return common.Address{}, err
	}
	return types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
}

// TxHash is the hash of a signed raw tx, known before (and independent of) broadcasting it.
func TxHash(rawTx string) (common.Hash, error) {
	b, err := hexutil.Decode(rawTx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("decoding raw tx: %w", err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		return common.Hash{}, fmt.Errorf("decoding raw tx: %w", err)
	}
	return tx.Hash(), nil
}

type Eth struct{ c *ethclient.Client }

func Dial(ctx context.Context, url string) (*Eth, error) {
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("dialing RPC: %w", err)
	}
	return &Eth{c: c}, nil
}

func (e *Eth) ChainID(ctx context.Context) (int64, error) {
	id, err := e.c.ChainID(ctx)
	if err != nil {
		return 0, err
	}
	return id.Int64(), nil
}

func (e *Eth) Head(ctx context.Context) (int64, error) {
	n, err := e.c.BlockNumber(ctx)
	return int64(n), err
}

func (e *Eth) Broadcast(ctx context.Context, rawTx string) error {
	b, err := hexutil.Decode(rawTx)
	if err != nil {
		return err
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		return err
	}
	err = e.c.SendTransaction(ctx, &tx)
	if err != nil && isAlreadyKnown(err) {
		return nil
	}
	return err
}

// isAlreadyKnown: the node already has this exact tx (a re-broadcast after a crash). Geth says
// "already known", Anvil and others "transaction already imported" / "known transaction".
func isAlreadyKnown(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already known") || strings.Contains(m, "already imported") || strings.Contains(m, "known transaction")
}

func (e *Eth) MinedNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return e.c.NonceAt(ctx, addr, nil)
}

func (e *Eth) BaseFee(ctx context.Context) (*big.Int, error) {
	h, err := e.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	if h.BaseFee == nil {
		return new(big.Int), nil
	}
	return h.BaseFee, nil
}

func (e *Eth) Balance(ctx context.Context, addr common.Address) (*big.Int, error) {
	return e.c.BalanceAt(ctx, addr, nil)
}

func (e *Eth) RevertReason(ctx context.Context, hash common.Hash, block int64) (string, error) {
	tx, _, err := e.c.TransactionByHash(ctx, hash)
	if err != nil {
		return "", fmt.Errorf("reading tx %s: %w", hash.Hex(), err)
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil {
		return "", err
	}
	// The state just before the tx's block; earlier txs in the same block are not replayed, so
	// this is a close approximation, which is all a log line needs.
	_, err = e.c.CallContract(ctx, ethereum.CallMsg{From: from, To: tx.To(), Gas: tx.Gas(), Value: tx.Value(), Data: tx.Data()},
		big.NewInt(block-1))
	if err == nil {
		return "", nil // succeeds on replay: the cause was something earlier in the same block
	}
	return err.Error(), nil
}

func (e *Eth) Receipt(ctx context.Context, hash common.Hash) (*Receipt, error) {
	r, err := e.c.TransactionReceipt(ctx, hash)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Receipt{BlockNumber: r.BlockNumber.Int64(), Success: r.Status == types.ReceiptStatusSuccessful}, nil
}

// Decode parses a signed raw tx.
func Decode(rawTx string) (*types.Transaction, error) {
	b, err := hexutil.Decode(rawTx)
	if err != nil {
		return nil, fmt.Errorf("decoding raw tx: %w", err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("decoding raw tx: %w", err)
	}
	return &tx, nil
}
