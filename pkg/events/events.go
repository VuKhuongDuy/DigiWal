// Package events holds the wire shape of every Kafka event in DigiWal, so a
// producer and its consumers are compiled against the same struct instead of
// each guessing field names and types (docs/design/integration-contracts.md §1).
// Structs and validation only — no business logic, no Kafka client.
//
// Conventions (all of them decided in the design docs unless noted):
//   - JSON on the wire.
//   - Amounts are decimal strings in minor units (Amount), never JSON numbers.
//   - Deposit events carry user_id, resolved once by chain-watcher, so no consumer has to map
//     an address to a user (deposit-flow.md, Address Resolution).
//   - Every event carries `version` (schema version, currently 1); Decode rejects
//     any version it doesn't know. Adding an optional field keeps version 1;
//     renaming/removing/retyping one bumps it. (Versioning scheme: dev choice,
//     not yet in a design doc.)
//   - Decode ignores unknown fields, so a producer can add a field without
//     breaking older consumers; Validate catches missing/malformed ones.
//   - IdempotencyKey() is the natural key a consumer dedups on, and Key() is
//     the Kafka message key (same value, so all messages for one deposit or
//     withdrawal land on one partition, in order).
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Version is the current schema version of every event in this package.
const Version = 1

// Topic names (docs/ARCHITECTURE.md; withdrawals.requested and transfers.internal
// are deliberately absent — see integration-contracts.md §1).
const (
	TopicDepositsDetected     = "deposits.detected"
	TopicDepositsConfirmed    = "deposits.confirmed"
	TopicWithdrawalsBroadcast = "withdrawals.broadcast"
	TopicWithdrawalsConfirmed = "withdrawals.confirmed"
)

// NativeLogIndex is the log_index of a native-coin deposit: no log exists to
// index, so -1 is the sentinel (integration-contracts.md, Open Questions #2).
const NativeLogIndex = -1

// Event is implemented by every payload in this package.
type Event interface {
	// Topic is the Kafka topic this event is published to.
	Topic() string
	// Key is the Kafka message key and the consumer's idempotency key.
	Key() string
	// Validate reports the first thing wrong with the payload, or nil.
	Validate() error
}

// Amount is a non-negative integer in an asset's minor units, carried as a
// decimal string so it survives JSON without int64/float64 loss.
type Amount string

func (a Amount) Validate() error {
	if a == "" {
		return errors.New("amount is empty")
	}
	for _, c := range a {
		if c < '0' || c > '9' {
			return fmt.Errorf("amount %q is not a non-negative decimal integer", string(a))
		}
	}
	if len(a) > 1 && a[0] == '0' {
		return fmt.Errorf("amount %q has leading zeros", string(a))
	}
	return nil
}

// Int returns the amount as a big.Int. Call Validate first.
func (a Amount) Int() *big.Int {
	n, _ := new(big.Int).SetString(string(a), 10)
	return n
}

// AmountFromInt renders n (must be >= 0) as an Amount.
func AmountFromInt(n *big.Int) Amount { return Amount(n.String()) }

var (
	hexAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	hexHash    = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// check collects the first validation error.
type check struct{ err error }

func (c *check) ok(cond bool, format string, args ...any) {
	if c.err == nil && !cond {
		c.err = fmt.Errorf(format, args...)
	}
}

func (c *check) common(version int) {
	c.ok(version == Version, "unsupported version %d (this build understands %d)", version, Version)
}
func (c *check) address(field, v string) {
	c.ok(hexAddress.MatchString(v), "%s %q is not a 0x-prefixed 20-byte hex address", field, v)
}
func (c *check) hash(field, v string) {
	c.ok(hexHash.MatchString(v), "%s %q is not a 0x-prefixed 32-byte hex hash", field, v)
}
func (c *check) uuid(field, v string) { c.ok(uuidRe.MatchString(v), "%s %q is not a uuid", field, v) }
func (c *check) nonEmpty(field, v string) {
	c.ok(strings.TrimSpace(v) != "", "%s is required", field)
}
func (c *check) amount(a Amount) {
	if c.err == nil {
		c.err = a.Validate()
	}
}
func (c *check) positiveInt(field string, v int64) { c.ok(v > 0, "%s must be positive", field) }
func (c *check) nonNegInt(field string, v int64)   { c.ok(v >= 0, "%s must be >= 0", field) }
func (c *check) present(field string, t time.Time) {
	c.ok(!t.IsZero(), "%s is required", field)
}

// Encode validates e and marshals it to its JSON wire form.
func Encode(e Event) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s event: %w", e.Topic(), err)
	}
	return json.Marshal(e)
}

// Decode parses data into a new T and validates it. Unknown fields are ignored;
// an unknown version or a missing/malformed required field is an error.
func Decode[T any, P interface {
	*T
	Event
}](data []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&v); err != nil {
		return v, fmt.Errorf("decoding event: %w", err)
	}
	if err := P(&v).Validate(); err != nil {
		return v, fmt.Errorf("invalid %s event: %w", P(&v).Topic(), err)
	}
	return v, nil
}

// depositKey is the natural key of a deposit: (chain_id, tx_hash, log_index)
// (ledger.md invariant #3, deposit-flow.md invariant #3).
func depositKey(chainID int64, txHash string, logIndex int64) string {
	return fmt.Sprintf("%d:%s:%d", chainID, strings.ToLower(txHash), logIndex)
}

// DepositDetected — chain-watcher → history-service. Informational only: the
// transfer was observed but has not reached Confirmation Depth, so it is never
// a balance change.
type DepositDetected struct {
	Version     int       `json:"version"`
	ChainID     int64     `json:"chain_id"`
	TxHash      string    `json:"tx_hash"`
	LogIndex    int64     `json:"log_index"` // NativeLogIndex (-1) for native coin
	AssetID     string    `json:"asset_id"`
	FromAddress string    `json:"from_address"`
	ToAddress   string    `json:"to_address"` // the Watched Address
	UserID      string    `json:"user_id"`    // owner of ToAddress, resolved by chain-watcher (Address Resolution)
	Amount      Amount    `json:"amount"`
	BlockNumber int64     `json:"block_number"`
	DetectedAt  time.Time `json:"detected_at"`
}

func (DepositDetected) Topic() string { return TopicDepositsDetected }
func (e DepositDetected) Key() string { return depositKey(e.ChainID, e.TxHash, e.LogIndex) }
func (e DepositDetected) Validate() error {
	var c check
	c.common(e.Version)
	c.positiveInt("chain_id", e.ChainID)
	c.hash("tx_hash", e.TxHash)
	c.ok(e.LogIndex >= NativeLogIndex, "log_index %d must be >= -1", e.LogIndex)
	c.uuid("asset_id", e.AssetID)
	c.address("from_address", e.FromAddress)
	c.address("to_address", e.ToAddress)
	c.nonEmpty("user_id", e.UserID)
	c.amount(e.Amount)
	c.nonNegInt("block_number", e.BlockNumber)
	c.present("detected_at", e.DetectedAt)
	return c.err
}

// DepositConfirmed — chain-watcher → ledger-service, sweeper, history-service.
// The deposit has reached Confirmation Depth; this is what credits a balance.
// from_address must be on the wire: both ledger-service and sweeper screen it
// (compliance-screening.md invariants #1, #2).
type DepositConfirmed struct {
	Version     int    `json:"version"`
	ChainID     int64  `json:"chain_id"`
	TxHash      string `json:"tx_hash"`
	LogIndex    int64  `json:"log_index"` // NativeLogIndex (-1) for native coin
	AssetID     string `json:"asset_id"`
	FromAddress string `json:"from_address"`
	ToAddress   string `json:"to_address"`
	UserID      string `json:"user_id"` // owner of ToAddress: the ledger credits (user_id, asset), never an address
	Amount      Amount `json:"amount"`
	BlockNumber int64  `json:"block_number"`
}

func (DepositConfirmed) Topic() string { return TopicDepositsConfirmed }
func (e DepositConfirmed) Key() string { return depositKey(e.ChainID, e.TxHash, e.LogIndex) }
func (e DepositConfirmed) Validate() error {
	var c check
	c.common(e.Version)
	c.positiveInt("chain_id", e.ChainID)
	c.hash("tx_hash", e.TxHash)
	c.ok(e.LogIndex >= NativeLogIndex, "log_index %d must be >= -1", e.LogIndex)
	c.uuid("asset_id", e.AssetID)
	c.address("from_address", e.FromAddress)
	c.address("to_address", e.ToAddress)
	c.nonEmpty("user_id", e.UserID)
	c.amount(e.Amount)
	c.nonNegInt("block_number", e.BlockNumber)
	return c.err
}

// WithdrawalBroadcast — withdrawal-service → history-service. The signed tx was
// sent; tx_hash and the Hot Wallet nonce it claimed are now known.
type WithdrawalBroadcast struct {
	Version        int       `json:"version"`
	IdempotencyKey string    `json:"idempotency_key"` // the Withdrawal Request's key, uuid
	UserID         string    `json:"user_id"`
	AssetID        string    `json:"asset_id"`
	Amount         Amount    `json:"amount"`
	ToAddress      string    `json:"to_address"`
	ChainID        int64     `json:"chain_id"`
	TxHash         string    `json:"tx_hash"`
	Nonce          int64     `json:"nonce"`
	BroadcastAt    time.Time `json:"broadcast_at"`
}

func (WithdrawalBroadcast) Topic() string { return TopicWithdrawalsBroadcast }
func (e WithdrawalBroadcast) Key() string { return e.IdempotencyKey }
func (e WithdrawalBroadcast) Validate() error {
	var c check
	c.common(e.Version)
	c.uuid("idempotency_key", e.IdempotencyKey)
	c.nonEmpty("user_id", e.UserID)
	c.uuid("asset_id", e.AssetID)
	c.amount(e.Amount)
	c.address("to_address", e.ToAddress)
	c.positiveInt("chain_id", e.ChainID)
	c.hash("tx_hash", e.TxHash)
	c.nonNegInt("nonce", e.Nonce)
	c.present("broadcast_at", e.BroadcastAt)
	return c.err
}

// WithdrawalConfirmed — withdrawal-service → history-service. The tx crossed
// Withdrawal Confirmation Depth. Not consumed by ledger-service: the debit
// already happened at Reservation time (withdrawal-flow.md invariant #1).
type WithdrawalConfirmed struct {
	Version        int       `json:"version"`
	IdempotencyKey string    `json:"idempotency_key"`
	UserID         string    `json:"user_id"`
	AssetID        string    `json:"asset_id"`
	Amount         Amount    `json:"amount"`
	ToAddress      string    `json:"to_address"`
	ChainID        int64     `json:"chain_id"`
	TxHash         string    `json:"tx_hash"`
	Nonce          int64     `json:"nonce"`
	BlockNumber    int64     `json:"block_number"` // block at which the depth was crossed
	ConfirmedAt    time.Time `json:"confirmed_at"`
}

func (WithdrawalConfirmed) Topic() string { return TopicWithdrawalsConfirmed }
func (e WithdrawalConfirmed) Key() string { return e.IdempotencyKey }
func (e WithdrawalConfirmed) Validate() error {
	var c check
	c.common(e.Version)
	c.uuid("idempotency_key", e.IdempotencyKey)
	c.nonEmpty("user_id", e.UserID)
	c.uuid("asset_id", e.AssetID)
	c.amount(e.Amount)
	c.address("to_address", e.ToAddress)
	c.positiveInt("chain_id", e.ChainID)
	c.hash("tx_hash", e.TxHash)
	c.nonNegInt("nonce", e.Nonce)
	c.nonNegInt("block_number", e.BlockNumber)
	c.present("confirmed_at", e.ConfirmedAt)
	return c.err
}
