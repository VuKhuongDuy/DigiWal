// Package history is the read model behind the future GET /v1/history: one denormalized
// document per deposit or withdrawal, fed by the four Kafka topics history-service consumes
// (integration-contracts.md §1, Open Questions #3). It holds no business rules and is never a
// source of truth — the ledger is; this can always be rebuilt by replaying the topics.
//
// The document shape is a dev proposal: history-service has no design doc yet (the plan's
// module map says so), so nothing here is a contract other services may rely on.
package history

import (
	"context"
	"fmt"
	"log"
	"time"

	"digiwal/pkg/events"
)

const (
	KindDeposit    = "deposit"
	KindWithdrawal = "withdrawal"

	StatusPending   = "pending"   // deposit seen, not yet confirmation-deep
	StatusBroadcast = "broadcast" // withdrawal sent, not yet confirmation-deep
	StatusConfirmed = "confirmed"
)

// Entry is one row of a user's history.
type Entry struct {
	ID          string // "<kind>:<event key>": unique across kinds
	UserID      string
	Kind        string
	AssetID     string
	Amount      string // decimal string in minor units; never a float, never int64
	Address     string // counterparty: the sender of a deposit, the destination of a withdrawal
	TxHash      string
	ChainID     int64
	BlockNumber int64
	// At is when the event happened: detection/broadcast time, or confirmation time.
	At time.Time
}

// Store persists entries. Both operations are idempotent: applying the same event twice,
// or events out of order, ends in the same state — a confirmed entry never goes back to
// pending/broadcast.
type Store interface {
	// RecordProgress inserts the entry in its in-flight status (pending/broadcast) unless one
	// already exists; it never modifies an existing entry.
	RecordProgress(ctx context.Context, e Entry, status string) error
	// RecordConfirmed inserts the entry as confirmed, or turns an existing one into confirmed.
	RecordConfirmed(ctx context.Context, e Entry) error
	// RecordFailedEvent parks an event that can never be processed.
	RecordFailedEvent(ctx context.Context, topic, key string, payload []byte, reason string) error
}

// Message is one Kafka record, independent of the client library.
type Message struct {
	Topic string
	Key   []byte
	Value []byte
	Raw   any
}

type Processor struct{ Store Store }

// Handle applies one record. nil means "done, safe to commit"; non-nil is transient and must
// be retried. Records that can never succeed (malformed, unknown topic) are parked in
// failed events and acknowledged so they cannot wedge the partition.
func (p *Processor) Handle(ctx context.Context, m Message) error {
	switch m.Topic {
	case events.TopicDepositsDetected:
		ev, err := events.Decode[events.DepositDetected](m.Value)
		if err != nil {
			return p.fail(ctx, m, err.Error())
		}
		return p.Store.RecordProgress(ctx, depositEntry(ev.ChainID, ev.TxHash, ev.LogIndex, ev.UserID, ev.AssetID, string(ev.Amount), ev.FromAddress, ev.BlockNumber, ev.DetectedAt), StatusPending)
	case events.TopicDepositsConfirmed:
		ev, err := events.Decode[events.DepositConfirmed](m.Value)
		if err != nil {
			return p.fail(ctx, m, err.Error())
		}
		// deposits.confirmed carries no timestamp; the time we learned of it is the closest honest value.
		return p.Store.RecordConfirmed(ctx, depositEntry(ev.ChainID, ev.TxHash, ev.LogIndex, ev.UserID, ev.AssetID, string(ev.Amount), ev.FromAddress, ev.BlockNumber, time.Now().UTC()))
	case events.TopicWithdrawalsBroadcast:
		ev, err := events.Decode[events.WithdrawalBroadcast](m.Value)
		if err != nil {
			return p.fail(ctx, m, err.Error())
		}
		return p.Store.RecordProgress(ctx, withdrawalEntry(ev.IdempotencyKey, ev.UserID, ev.AssetID, string(ev.Amount), ev.ToAddress, ev.TxHash, ev.ChainID, 0, ev.BroadcastAt), StatusBroadcast)
	case events.TopicWithdrawalsConfirmed:
		ev, err := events.Decode[events.WithdrawalConfirmed](m.Value)
		if err != nil {
			return p.fail(ctx, m, err.Error())
		}
		return p.Store.RecordConfirmed(ctx, withdrawalEntry(ev.IdempotencyKey, ev.UserID, ev.AssetID, string(ev.Amount), ev.ToAddress, ev.TxHash, ev.ChainID, ev.BlockNumber, ev.ConfirmedAt))
	default:
		return p.fail(ctx, m, fmt.Sprintf("unexpected topic %q", m.Topic))
	}
}

func depositEntry(chainID int64, txHash string, logIndex int64, user, asset, amount, from string, block int64, at time.Time) Entry {
	return Entry{ID: KindDeposit + ":" + depositKey(chainID, txHash, logIndex), UserID: user, Kind: KindDeposit,
		AssetID: asset, Amount: amount, Address: from, TxHash: txHash, ChainID: chainID, BlockNumber: block, At: at.UTC()}
}

func withdrawalEntry(key, user, asset, amount, to, txHash string, chainID, block int64, at time.Time) Entry {
	return Entry{ID: KindWithdrawal + ":" + key, UserID: user, Kind: KindWithdrawal,
		AssetID: asset, Amount: amount, Address: to, TxHash: txHash, ChainID: chainID, BlockNumber: block, At: at.UTC()}
}

func depositKey(chainID int64, txHash string, logIndex int64) string {
	return fmt.Sprintf("%d:%s:%d", chainID, lower(txHash), logIndex)
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (p *Processor) fail(ctx context.Context, m Message, reason string) error {
	if err := p.Store.RecordFailedEvent(ctx, m.Topic, string(m.Key), m.Value, reason); err != nil {
		return err // could not even park it: retry
	}
	log.Printf("parked unprocessable %s event %q: %s", m.Topic, m.Key, reason)
	return nil
}

// Reader is the consumer side of Kafka, so Run is testable without a broker.
type Reader interface {
	Fetch(ctx context.Context) (Message, error)
	Commit(ctx context.Context, m Message) error
}

// Run reads until ctx ends. A record is committed only after Handle succeeded (at-least-once);
// a transient failure backs off and retries the same record, up to maxAttempts, then logs an
// ALERT and skips it — the offset still advances (integration-contracts.md §5). Safe because
// nothing depends on this read model: a lost row is one missing line of history, never a balance.
func Run(ctx context.Context, r Reader, p *Processor, maxAttempts int) error {
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
		for attempt := 1; ; attempt++ {
			err := p.Handle(ctx, m)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			if attempt >= maxAttempts {
				log.Printf("ALERT: giving up on %s record %q after %d attempts, skipping it: %v", m.Topic, m.Key, attempt, err)
				break
			}
			log.Printf("transient failure handling %s (attempt %d/%d, will retry): %v", m.Topic, attempt, maxAttempts, err)
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
