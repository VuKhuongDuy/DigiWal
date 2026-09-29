// Package consumer feeds Kafka events into the ledger. Today that is
// deposits.confirmed: screen the sender, then credit the user (or flag the deposit).
package consumer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/ledger-service/internal/ledger"
)

// ErrUnknownAddress: the deposit address belongs to no user we know of.
var ErrUnknownAddress = errors.New("unknown deposit address")

// UserResolver maps a deposit's to_address to the user who owns it.
//
// OPEN DESIGN GAP: nothing in docs/design says how ledger-service learns this.
// deposits.confirmed carries to_address but no user_id, and only wallet-service
// knows address -> user. Until the Solution Architect decides (add user_id to the
// event, have ledger call wallet-service, or replicate the mapping) there is no real
// implementation; main wires none and the consumer stays off.
type UserResolver interface {
	UserForAddress(ctx context.Context, chainID int64, address string) (string, error)
}

// Ledger is the slice of ledger.Store the processor uses.
type Ledger interface {
	Apply(ctx context.Context, t ledger.Transaction) (ledger.Status, error)
	InsertFlaggedDeposit(ctx context.Context, d ledger.FlaggedDeposit) (bool, error)
	RecordFailedEvent(ctx context.Context, topic, key string, payload []byte, reason string) error
}

// Screener is the Screening List.
type Screener interface{ Contains(address string) bool }

// Message is one Kafka record, independent of any client library.
type Message struct {
	Topic string
	Key   []byte
	Value []byte
	// Raw is the client library's own record, handed back to Reader.Commit.
	Raw any
}

// Reader is the consumer side of Kafka, so the processor is testable without a broker.
type Reader interface {
	Fetch(ctx context.Context) (Message, error)
	Commit(ctx context.Context, m Message) error
}

type Processor struct {
	Ledger    Ledger
	Screening Screener
	Users     UserResolver
}

// Handle processes one deposits.confirmed record. A nil return means "done, safe to
// commit the offset"; a non-nil return is transient (database down, resolver
// unreachable) and the message must be retried, not committed.
//
// Events that can never succeed — malformed, unregistered asset, unknown address,
// zero amount — are stored in failed_events and then acknowledged, because retrying
// them forever would wedge the partition behind one bad record.
func (p *Processor) Handle(ctx context.Context, m Message) error {
	ev, err := events.Decode[events.DepositConfirmed](m.Value)
	if err != nil {
		return p.fail(ctx, m, "", err.Error())
	}
	amount := ev.Amount.Int()
	if amount.Sign() == 0 {
		return p.fail(ctx, m, ev.Key(), "zero-amount deposit")
	}

	// compliance-screening.md invariant #1: checked before apply(), and a match never
	// becomes a Ledger Transaction — it is held for operator review instead.
	if p.Screening.Contains(ev.FromAddress) {
		created, err := p.Ledger.InsertFlaggedDeposit(ctx, ledger.FlaggedDeposit{
			ChainID: ev.ChainID, TxHash: ev.TxHash, LogIndex: ev.LogIndex, AssetID: ev.AssetID,
			FromAddress: ev.FromAddress, ToAddress: ev.ToAddress, Amount: amount, BlockNumber: ev.BlockNumber,
		})
		if err != nil {
			return err
		}
		if created {
			log.Printf("deposit %s flagged: sender %s is on the Screening List", ev.Key(), ev.FromAddress)
		}
		return nil
	}

	if p.Users == nil {
		return errors.New("no UserResolver configured: cannot map deposit address to a user")
	}
	userID, err := p.Users.UserForAddress(ctx, ev.ChainID, ev.ToAddress)
	if errors.Is(err, ErrUnknownAddress) {
		return p.fail(ctx, m, ev.Key(), fmt.Sprintf("no user owns deposit address %s", ev.ToAddress))
	}
	if err != nil {
		return fmt.Errorf("resolving user for %s: %w", ev.ToAddress, err)
	}

	tx := ledger.Transaction{
		IdempotencyKey: ev.Key(), // (chain_id, tx_hash, log_index), ledger.md invariant #3
		Source:         ledger.SourceDeposit,
		Deposit:        &ledger.DepositRef{ChainID: ev.ChainID, TxHash: ev.TxHash, LogIndex: ev.LogIndex},
		Entries: []ledger.Entry{
			{Ref: ledger.RefUser, UserID: userID, AssetID: ev.AssetID, Amount: amount},
			{Ref: ledger.RefChain, AssetID: ev.AssetID, Amount: new(big.Int).Neg(amount)},
		},
	}
	switch _, err := p.Ledger.Apply(ctx, tx); {
	case err == nil: // applied, or already applied by an earlier delivery: both are success
		return nil
	case errors.Is(err, ledger.ErrUnknownAsset), errors.Is(err, ledger.ErrInvalid), errors.Is(err, ledger.ErrIdempotencyKeyReused):
		return p.fail(ctx, m, ev.Key(), err.Error())
	default:
		return err
	}
}

func (p *Processor) fail(ctx context.Context, m Message, key, reason string) error {
	if key == "" {
		key = string(m.Key)
	}
	if err := p.Ledger.RecordFailedEvent(ctx, m.Topic, key, m.Value, reason); err != nil {
		return err // could not even park it: retry
	}
	log.Printf("parked unprocessable %s event %q: %s", m.Topic, key, reason)
	return nil
}

// Run reads until ctx is cancelled. A record is committed only after Handle succeeds
// (at-least-once); transient failures back off and retry the same record.
func Run(ctx context.Context, r Reader, p *Processor) error {
	const maxBackoff = 30 * time.Second
	for {
		m, err := r.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("fetching: %w", err)
		}
		backoff := 500 * time.Millisecond
		for {
			err := p.Handle(ctx, m)
			if err == nil {
				break
			}
			log.Printf("transient failure handling %s (will retry): %v", m.Topic, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
		if err := r.Commit(ctx, m); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("committing: %w", err)
		}
	}
}
