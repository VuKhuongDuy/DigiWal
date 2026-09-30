// Package chain is chain-watcher's view of an EVM node: only the reads it needs, behind an
// interface so the watcher's logic (reorgs, confirmation, resolution failures) is testable
// without a node. chain-watcher only ever reads the chain; it never signs or broadcasts.
package chain

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

type Tx struct {
	Hash  common.Hash
	From  common.Address
	To    *common.Address // nil for contract creation
	Value *big.Int
}

type Block struct {
	Number     int64
	Hash       common.Hash
	ParentHash common.Hash
	Txs        []Tx
}

// Log is a matching log with just the fields chain-watcher reads.
type Log struct {
	Address     common.Address
	Topics      []common.Hash
	Data        []byte
	BlockNumber int64
	BlockHash   common.Hash
	TxHash      common.Hash
	Index       int64 // index within the block
}

type Receipt struct {
	BlockNumber int64
	BlockHash   common.Hash
	Success     bool
}

type Chain interface {
	ChainID(ctx context.Context) (int64, error)
	// Head is the number of the latest block.
	Head(ctx context.Context) (int64, error)
	// BlockHash returns the hash of block n on the node's current canonical chain;
	// ok is false when the node has no block n (yet, or any more after a reorg).
	BlockHash(ctx context.Context, n int64) (h common.Hash, ok bool, err error)
	// Block returns block n with its transactions; nil when the node has no block n.
	Block(ctx context.Context, n int64) (*Block, error)
	// Logs returns logs emitted by contracts in [from, to] whose first topic is topic0.
	Logs(ctx context.Context, from, to int64, contracts []common.Address, topic0 common.Hash) ([]Log, error)
	// Receipt returns the transaction's receipt on the canonical chain; nil when unknown.
	Receipt(ctx context.Context, tx common.Hash) (*Receipt, error)
}
