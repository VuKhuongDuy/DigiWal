package sweep

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"digiwal/pkg/events"

	"github.com/segmentio/kafka-go"
)

// Message is one Kafka record, independent of the client library.
type Message struct {
	Topic string
	Key   []byte
	Value []byte
	Raw   any
}

// Reader is the consumer side of Kafka, so Run is testable without a broker.
type Reader interface {
	Fetch(ctx context.Context) (Message, error)
	Commit(ctx context.Context, m Message) error
}

// Handle sweeps one deposits.confirmed record. An undecodable record can never succeed and is
// reported as ErrPermanent.
func (s *Sweeper) Handle(ctx context.Context, m Message) error {
	ev, err := events.Decode[events.DepositConfirmed](m.Value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return s.Sweep(ctx, ev)
}

// Run reads until ctx ends. A record is committed only after it was swept (at-least-once).
// Transient failures back off and retry; after maxAttempts — or at once for a permanent one —
// the record is given up on with a loud ALERT and committed, so one stuck address cannot halt
// every later sweep. Giving up is safe for funds: the deposit stays credited in the ledger and
// stays at the user address, and the next deposit event for that address sweeps its whole
// balance. sweeper has no store, so there is no dead-letter yet (an open gap).
func Run(ctx context.Context, r Reader, s *Sweeper, maxAttempts int) error {
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
			err := s.Handle(ctx, m)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrPermanent) {
				log.Printf("ALERT: not sweeping %s record %q, it can never succeed: %v", m.Topic, m.Key, err)
				orNop(s.Observer).Abandoned()
				break
			}
			if attempt >= maxAttempts {
				log.Printf("ALERT: giving up sweeping %s record %q after %d attempts, funds stay at the user address: %v", m.Topic, m.Key, attempt, err)
				orNop(s.Observer).Abandoned()
				break
			}
			log.Printf("sweep %s record %q failed (attempt %d/%d, will retry): %v", m.Topic, m.Key, attempt, maxAttempts, err)
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

// KafkaReader is a consumer-group reader over kafka-go.
type KafkaReader struct{ r *kafka.Reader }

func NewKafkaReader(brokers []string, groupID, topic string) *KafkaReader {
	return &KafkaReader{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers, GroupID: groupID, Topic: topic,
		MinBytes: 1, MaxBytes: 10 << 20,
		StartOffset: kafka.FirstOffset, // a new group must not skip deposits already published
	})}
}

func (k *KafkaReader) Fetch(ctx context.Context) (Message, error) {
	m, err := k.r.FetchMessage(ctx)
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: m.Topic, Key: m.Key, Value: m.Value, Raw: m}, nil
}

func (k *KafkaReader) Commit(ctx context.Context, m Message) error {
	raw, ok := m.Raw.(kafka.Message)
	if !ok {
		return fmt.Errorf("message was not fetched by this reader")
	}
	return k.r.CommitMessages(ctx, raw)
}

func (k *KafkaReader) Close() error { return k.r.Close() }
