package history

import (
	"context"
	"fmt"

	"github.com/segmentio/kafka-go"
)

// KafkaReader is one consumer group over several topics.
type KafkaReader struct{ r *kafka.Reader }

func NewKafkaReader(brokers []string, groupID string, topics []string) *KafkaReader {
	return &KafkaReader{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers, GroupID: groupID, GroupTopics: topics,
		MinBytes: 1, MaxBytes: 10 << 20,
		StartOffset: kafka.FirstOffset, // a new group replays from the start: the read model is rebuilt from the topics
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
