package consumer

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
)

// KafkaReader is a consumer-group reader over kafka-go (chosen in the plan over
// confluent-kafka-go: no cgo).
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
