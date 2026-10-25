package sweep

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// RPCChain adapts go-ethereum's ethclient to Chain.
type RPCChain struct{ c *ethclient.Client }

// DialRPC connects to the EVM RPC.
func DialRPC(ctx context.Context, url string) (*RPCChain, error) {
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("dialing EVM RPC: %w", err)
	}
	return &RPCChain{c: c}, nil
}

func (r *RPCChain) ChainID(ctx context.Context) (*big.Int, error) { return r.c.ChainID(ctx) }

func (r *RPCChain) PendingNonceAt(ctx context.Context, a common.Address) (uint64, error) {
	return r.c.PendingNonceAt(ctx, a)
}

func (r *RPCChain) BalanceAt(ctx context.Context, a common.Address) (*big.Int, error) {
	return r.c.BalanceAt(ctx, a, nil)
}

func (r *RPCChain) FeeQuote(ctx context.Context) (*big.Int, *big.Int, error) {
	tip, err := r.c.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}
	head, err := r.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if head.BaseFee == nil {
		return nil, nil, fmt.Errorf("chain has no EIP-1559 base fee")
	}
	return tip, head.BaseFee, nil
}

func (r *RPCChain) EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error) {
	return r.c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: value, Data: data})
}
