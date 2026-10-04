package source

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"digiwal/services/auditor/internal/rules/deposit"
	"digiwal/services/auditor/internal/rules/withdrawal"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
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

// Candidate is a transfer found on chain that credits an address.
type Candidate struct {
	TxHash   string
	LogIndex int64 // -1 for the native coin
	To       string
	Amount   *big.Int
}

// DepositChain is the chain access the D rules need. Read-only.
type DepositChain interface {
	// Transfer reads the transfer (tx_hash, log_index): the tx itself for log_index -1, else the
	// Transfer log with that block-wide index in the tx's receipt.
	Transfer(ctx context.Context, txHash string, logIndex int64) (deposit.OnChain, error)
	// BlockTime is the timestamp of block n.
	BlockTime(ctx context.Context, n int64) (time.Time, error)
	// Scan lists successful transfers in blocks [from, to] that go to an address isDeposit accepts: ERC-20
	// Transfer logs of the given token contracts, and plain native value transfers.
	Scan(ctx context.Context, from, to int64, tokens []string, isDeposit func(addr string) bool) ([]Candidate, error)
}

var transferSig = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

func (e EthChain) Transfer(ctx context.Context, txHash string, logIndex int64) (deposit.OnChain, error) {
	h := common.HexToHash(txHash)
	tx, _, err := e.C.TransactionByHash(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return deposit.OnChain{}, nil
	}
	if err != nil {
		return deposit.OnChain{}, fmt.Errorf("rpc: transaction %s: %w", txHash, err)
	}
	rcpt, err := e.C.TransactionReceipt(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return deposit.OnChain{}, nil // not mined: the transfer does not exist (yet)
	}
	if err != nil {
		return deposit.OnChain{}, fmt.Errorf("rpc: receipt %s: %w", txHash, err)
	}
	out := deposit.OnChain{Success: rcpt.Status == 1, Block: rcpt.BlockNumber.Int64(), BlockHash: strings.ToLower(rcpt.BlockHash.Hex())}
	hdr, err := e.C.HeaderByNumber(ctx, rcpt.BlockNumber)
	if err != nil {
		return deposit.OnChain{}, fmt.Errorf("rpc: header %d: %w", out.Block, err)
	}
	out.Canonical = strings.ToLower(hdr.Hash().Hex())

	if logIndex < 0 {
		out.Found, out.Amount = true, new(big.Int).Set(tx.Value())
		if tx.To() != nil {
			out.To = strings.ToLower(tx.To().Hex())
		}
		return out, nil
	}
	for _, l := range rcpt.Logs {
		if int64(l.Index) == logIndex && len(l.Topics) == 3 && l.Topics[0] == transferSig {
			out.Found, out.Token = true, strings.ToLower(l.Address.Hex())
			out.To = strings.ToLower(common.BytesToAddress(l.Topics[2].Bytes()).Hex())
			out.Amount = new(big.Int).SetBytes(l.Data)
			return out, nil
		}
	}
	return out, nil // mined, but it has no such Transfer log
}

func (e EthChain) BlockTime(ctx context.Context, n int64) (time.Time, error) {
	h, err := e.C.HeaderByNumber(ctx, big.NewInt(n))
	if err != nil {
		return time.Time{}, fmt.Errorf("rpc: header %d: %w", n, err)
	}
	return time.Unix(int64(h.Time), 0).UTC(), nil
}

func (e EthChain) Scan(ctx context.Context, from, to int64, tokens []string, isDeposit func(string) bool) ([]Candidate, error) {
	var out []Candidate
	if len(tokens) > 0 {
		var addrs []common.Address
		for _, t := range tokens {
			addrs = append(addrs, common.HexToAddress(t))
		}
		logs, err := e.C.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: big.NewInt(from), ToBlock: big.NewInt(to), Addresses: addrs, Topics: [][]common.Hash{{transferSig}},
		})
		if err != nil {
			return nil, fmt.Errorf("rpc: logs %d-%d: %w", from, to, err)
		}
		for _, l := range logs {
			if l.Removed || len(l.Topics) != 3 {
				continue
			}
			if dst := strings.ToLower(common.BytesToAddress(l.Topics[2].Bytes()).Hex()); isDeposit(dst) {
				out = append(out, Candidate{TxHash: strings.ToLower(l.TxHash.Hex()), LogIndex: int64(l.Index), To: dst, Amount: new(big.Int).SetBytes(l.Data)})
			}
		}
	}
	for n := from; n <= to; n++ {
		b, err := e.C.BlockByNumber(ctx, big.NewInt(n))
		if err != nil {
			return nil, fmt.Errorf("rpc: block %d: %w", n, err)
		}
		for _, tx := range b.Transactions() {
			if tx.To() == nil || tx.Value().Sign() <= 0 {
				continue
			}
			dst := strings.ToLower(tx.To().Hex())
			if !isDeposit(dst) {
				continue
			}
			r, err := e.C.TransactionReceipt(ctx, tx.Hash())
			if err != nil {
				return nil, fmt.Errorf("rpc: receipt %s: %w", tx.Hash().Hex(), err)
			}
			if r.Status == 1 {
				out = append(out, Candidate{TxHash: strings.ToLower(tx.Hash().Hex()), LogIndex: -1, To: dst, Amount: new(big.Int).Set(tx.Value())})
			}
		}
	}
	return out, nil
}
