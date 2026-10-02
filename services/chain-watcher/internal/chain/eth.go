package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Client is the slice of go-ethereum's client that Eth uses; *ethclient.Client satisfies it,
// and so does the in-process simulated backend the tests run against.
type Client interface {
	ChainID(ctx context.Context) (*big.Int, error)
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// Eth is Chain over go-ethereum's client (Sepolia, Anvil, any JSON-RPC node).
type Eth struct {
	c       Client
	chainID *big.Int
	signer  types.Signer
}

// Dial connects to rpcURL and reads the node's chain id once.
func Dial(ctx context.Context, rpcURL string) (*Eth, error) {
	c, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", rpcURL, err)
	}
	return NewEth(ctx, c)
}

func NewEth(ctx context.Context, c Client) (*Eth, error) {
	id, err := c.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading chain id: %w", err)
	}
	return &Eth{c: c, chainID: id, signer: types.LatestSignerForChainID(id)}, nil
}

func (e *Eth) ChainID(context.Context) (int64, error) { return e.chainID.Int64(), nil }

func (e *Eth) Head(ctx context.Context) (int64, error) {
	n, err := e.c.BlockNumber(ctx)
	return int64(n), err
}

func (e *Eth) BlockHash(ctx context.Context, n int64) (common.Hash, bool, error) {
	h, err := e.c.HeaderByNumber(ctx, big.NewInt(n))
	if errors.Is(err, ethereum.NotFound) {
		return common.Hash{}, false, nil
	}
	if err != nil {
		return common.Hash{}, false, err
	}
	return h.Hash(), true, nil
}

func (e *Eth) Block(ctx context.Context, n int64) (*Block, error) {
	b, err := e.c.BlockByNumber(ctx, big.NewInt(n))
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &Block{Number: n, Hash: b.Hash(), ParentHash: b.ParentHash(), Txs: make([]Tx, 0, len(b.Transactions()))}
	for _, tx := range b.Transactions() {
		from, err := types.Sender(e.signer, tx)
		if err != nil {
			// A tx type we cannot recover a sender for cannot be a plain deposit to us.
			continue
		}
		out.Txs = append(out.Txs, Tx{Hash: tx.Hash(), From: from, To: tx.To(), Value: tx.Value()})
	}
	return out, nil
}

func (e *Eth) Logs(ctx context.Context, from, to int64, contracts []common.Address, topic0 common.Hash) ([]Log, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	raw, err := e.c.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: big.NewInt(from), ToBlock: big.NewInt(to),
		Addresses: contracts, Topics: [][]common.Hash{{topic0}},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Log, 0, len(raw))
	for _, l := range raw {
		if l.Removed {
			continue
		}
		out = append(out, Log{
			Address: l.Address, Topics: l.Topics, Data: l.Data,
			BlockNumber: int64(l.BlockNumber), BlockHash: l.BlockHash, TxHash: l.TxHash, Index: int64(l.Index),
		})
	}
	return out, nil
}

func (e *Eth) Receipt(ctx context.Context, tx common.Hash) (*Receipt, error) {
	r, err := e.c.TransactionReceipt(ctx, tx)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Receipt{BlockNumber: r.BlockNumber.Int64(), BlockHash: r.BlockHash, Success: r.Status == types.ReceiptStatusSuccessful}, nil
}
