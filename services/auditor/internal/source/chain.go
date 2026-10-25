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
	"github.com/ethereum/go-ethereum/core/types"
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
	if rcpt.EffectiveGasPrice != nil {
		out.GasFee = new(big.Int).Mul(new(big.Int).SetUint64(rcpt.GasUsed), rcpt.EffectiveGasPrice)
	}
	return out, nil
}

// Candidate is a transfer found on chain that credits an address.
type Candidate struct {
	TxHash   string
	LogIndex int64  // -1 for the native coin
	From     string // the sender of a native transfer; empty for an ERC-20 Transfer log
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
		if f, ok := sender(tx); ok {
			out.From = strings.ToLower(f.Hex())
		}
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
				from := ""
				if f, ok := sender(tx); ok {
					from = strings.ToLower(f.Hex())
				}
				out = append(out, Candidate{TxHash: strings.ToLower(tx.Hash().Hex()), LogIndex: -1, From: from, To: dst, Amount: new(big.Int).Set(tx.Value())})
			}
		}
	}
	return out, nil
}

// RawMovement is a transfer with a tier address on one side, as found on chain.
type RawMovement struct {
	TxHash   string
	LogIndex int64 // -1 native
	Token    string
	From, To string
	Amount   *big.Int
	Block    int64
	Time     time.Time
}

// SentTx is a mined transaction sent by the Hot address.
type SentTx struct {
	Hash  string
	Nonce uint64
	Block int64
}

// TreasuryChain is the chain access the T rules need. Read-only.
type TreasuryChain interface {
	Head(ctx context.Context) (int64, error)
	BlockTime(ctx context.Context, n int64) (time.Time, error)
	// TxCount is the Hot address's transaction count at the latest block.
	TxCount(ctx context.Context, addr string) (uint64, error)
	// ScanTreasury lists, for blocks [from, to]: every tx sent by hot, and every successful transfer
	// (native value, or ERC-20 Transfer of the given tokens) that has one of tiers as sender or recipient.
	ScanTreasury(ctx context.Context, from, to int64, hot string, tiers, tokens []string) ([]RawMovement, []SentTx, error)
	// Sent describes one tx: whether it exists and mined, and if sent by hot, its nonce.
	Sent(ctx context.Context, hash, hot string) (found, fromHot bool, nonce uint64, err error)
}

func (e EthChain) Head(ctx context.Context) (int64, error) {
	n, err := e.C.BlockNumber(ctx)
	return int64(n), err
}

func (e EthChain) TxCount(ctx context.Context, addr string) (uint64, error) {
	return e.C.NonceAt(ctx, common.HexToAddress(addr), nil)
}

func sender(tx *types.Transaction) (common.Address, bool) {
	signer := types.LatestSignerForChainID(tx.ChainId())
	from, err := types.Sender(signer, tx)
	return from, err == nil
}

func (e EthChain) Sent(ctx context.Context, hash, hot string) (bool, bool, uint64, error) {
	h := common.HexToHash(hash)
	tx, _, err := e.C.TransactionByHash(ctx, h)
	if errors.Is(err, ethereum.NotFound) {
		return false, false, 0, nil
	}
	if err != nil {
		return false, false, 0, fmt.Errorf("rpc: transaction %s: %w", hash, err)
	}
	if _, err := e.C.TransactionReceipt(ctx, h); errors.Is(err, ethereum.NotFound) {
		return false, false, 0, nil // not mined: gone (a reorg) or never was
	} else if err != nil {
		return false, false, 0, fmt.Errorf("rpc: receipt %s: %w", hash, err)
	}
	from, ok := sender(tx)
	return true, ok && strings.EqualFold(from.Hex(), hot), tx.Nonce(), nil
}

func (e EthChain) ScanTreasury(ctx context.Context, from, to int64, hot string, tiers, tokens []string) ([]RawMovement, []SentTx, error) {
	tierSet := map[string]bool{}
	var tierHashes []common.Hash
	for _, t := range tiers {
		tierSet[strings.ToLower(t)] = true
		tierHashes = append(tierHashes, common.BytesToHash(common.HexToAddress(t).Bytes()))
	}
	var moves []RawMovement
	var sent []SentTx
	times := map[int64]time.Time{}

	for n := from; n <= to; n++ {
		b, err := e.C.BlockByNumber(ctx, big.NewInt(n))
		if err != nil {
			return nil, nil, fmt.Errorf("rpc: block %d: %w", n, err)
		}
		bt := time.Unix(int64(b.Time()), 0).UTC()
		times[n] = bt
		for _, tx := range b.Transactions() {
			f, ok := sender(tx)
			if !ok {
				continue
			}
			fromAddr := strings.ToLower(f.Hex())
			if fromAddr == strings.ToLower(hot) {
				sent = append(sent, SentTx{Hash: strings.ToLower(tx.Hash().Hex()), Nonce: tx.Nonce(), Block: n})
			}
			if tx.To() == nil || tx.Value().Sign() <= 0 {
				continue
			}
			toAddr := strings.ToLower(tx.To().Hex())
			if !tierSet[fromAddr] && !tierSet[toAddr] {
				continue
			}
			r, err := e.C.TransactionReceipt(ctx, tx.Hash())
			if err != nil {
				return nil, nil, fmt.Errorf("rpc: receipt %s: %w", tx.Hash().Hex(), err)
			}
			if r.Status == 1 {
				moves = append(moves, RawMovement{TxHash: strings.ToLower(tx.Hash().Hex()), LogIndex: -1, From: fromAddr, To: toAddr,
					Amount: new(big.Int).Set(tx.Value()), Block: n, Time: bt})
			}
		}
	}

	if len(tokens) > 0 && len(tierHashes) > 0 {
		var addrs []common.Address
		for _, t := range tokens {
			addrs = append(addrs, common.HexToAddress(t))
		}
		seen := map[string]bool{}
		for _, topics := range [][][]common.Hash{{{transferSig}, tierHashes}, {{transferSig}, nil, tierHashes}} {
			logs, err := e.C.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: big.NewInt(from), ToBlock: big.NewInt(to), Addresses: addrs, Topics: topics})
			if err != nil {
				return nil, nil, fmt.Errorf("rpc: logs %d-%d: %w", from, to, err)
			}
			for _, l := range logs {
				k := fmt.Sprintf("%s:%d", l.TxHash.Hex(), l.Index)
				if l.Removed || len(l.Topics) != 3 || seen[k] {
					continue
				}
				seen[k] = true
				bn := int64(l.BlockNumber)
				moves = append(moves, RawMovement{TxHash: strings.ToLower(l.TxHash.Hex()), LogIndex: int64(l.Index), Token: strings.ToLower(l.Address.Hex()),
					From: strings.ToLower(common.BytesToAddress(l.Topics[1].Bytes()).Hex()), To: strings.ToLower(common.BytesToAddress(l.Topics[2].Bytes()).Hex()),
					Amount: new(big.Int).SetBytes(l.Data), Block: bn, Time: times[bn]})
			}
		}
	}
	return moves, sent, nil
}

// ReconChain is the chain access the reconciliation needs. Read-only.
type ReconChain interface {
	Head(ctx context.Context) (int64, error)
	// Balance is addr's balance at the given block: the native coin if token is "", else the ERC-20 balance.
	Balance(ctx context.Context, addr, token string, block int64) (*big.Int, error)
}

var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31}

func (e EthChain) Balance(ctx context.Context, addr, token string, block int64) (*big.Int, error) {
	b := big.NewInt(block)
	if token == "" {
		v, err := e.C.BalanceAt(ctx, common.HexToAddress(addr), b)
		if err != nil {
			return nil, fmt.Errorf("rpc: balance of %s at %d: %w", addr, block, err)
		}
		return v, nil
	}
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(common.HexToAddress(addr).Bytes(), 32)...)
	t := common.HexToAddress(token)
	out, err := e.C.CallContract(ctx, ethereum.CallMsg{To: &t, Data: data}, b)
	if err != nil {
		return nil, fmt.Errorf("rpc: balanceOf %s on %s at %d: %w", addr, token, block, err)
	}
	return new(big.Int).SetBytes(out), nil
}
