package source

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"digiwal/services/auditor/internal/rules/withdrawal"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Chain reads transactions from the EVM RPC. It only reads: nothing here can send a transaction.
type Chain interface {
	Tx(ctx context.Context, hash string) (withdrawal.ChainTx, error)
}

type EthChain struct{ C *ethclient.Client }

// Tx returns what the node knows about hash. A hash the node does not know is Found=false, not an error;
// an RPC failure is an error (the job then ends in `error`, never `ok`).
func (e EthChain) Tx(ctx context.Context, hash string) (withdrawal.ChainTx, error) {
	h := common.HexToHash(hash)
	tx, _, err := e.C.TransactionByHash(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return withdrawal.ChainTx{}, nil
	}
	if err != nil {
		return withdrawal.ChainTx{}, fmt.Errorf("rpc: transaction %s: %w", hash, err)
	}
	out := withdrawal.ChainTx{Found: true, Value: new(big.Int).Set(tx.Value()), Input: tx.Data()}
	if tx.To() != nil {
		out.To = strings.ToLower(tx.To().Hex())
	}
	rcpt, err := e.C.TransactionReceipt(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return out, nil // known but not mined (yet)
	}
	if err != nil {
		return withdrawal.ChainTx{}, fmt.Errorf("rpc: receipt %s: %w", hash, err)
	}
	out.Mined, out.Success, out.Block = true, rcpt.Status == 1, rcpt.BlockNumber.Int64()
	return out, nil
}
