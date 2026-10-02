// Package publisher sends chain-watcher's two events to Kafka.
package publisher

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
)

// Publisher sends one message and returns only once the broker has acknowledged it.
type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

// Kafka is a kafka-go writer. Messages with the same key (one deposit) hash to one
// partition, so a consumer sees a deposit's detected -> confirmed in order.
type Kafka struct{ w *kafka.Writer }

func NewKafka(brokers []string) *Kafka {
	return &Kafka{w: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll, // an acknowledged deposit event must not vanish with a broker
		// deposits.* topics are created on first publish on a fresh dev broker.
		AllowAutoTopicCreation: true,
	}}
}

func (k *Kafka) Publish(ctx context.Context, topic, key string, value []byte) error {
	if err := k.w.WriteMessages(ctx, kafka.Message{Topic: topic, Key: []byte(key), Value: value}); err != nil {
		return fmt.Errorf("publishing to %s: %w", topic, err)
	}
	return nil
}

func (k *Kafka) Close() error { return k.w.Close() }
