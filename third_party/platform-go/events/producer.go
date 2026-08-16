package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/segmentio/kafka-go"
)

// ProducerConfig configures a Producer.
type ProducerConfig struct {
	Brokers  []string
	ClientID string
}

// Producer publishes Envelope messages to Kafka with strong durability defaults
// (RequireAll, synchronous writes) suitable for finance/audit/outcome topics.
type Producer struct {
	writer *kafka.Writer
}

// NewProducer constructs a Producer using one shared writer (topic is set
// per-message). Acks default to RequireAll for durability.
func NewProducer(cfg ProducerConfig) *Producer {
	return &Producer{
		writer: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Balancer:     &kafka.Hash{}, // deterministic per-key partitioning
			RequiredAcks: kafka.RequireAll,
			Async:        false,
			Transport: &kafka.Transport{
				ClientID: cfg.ClientID,
			},
		},
	}
}

// Publish writes one envelope to the named topic. Key is used for partitioning
// (typically the aggregate id). Pass an empty key for round-robin distribution.
func (p *Producer) Publish(ctx context.Context, topic, key string, env Envelope) error {
	if p == nil || p.writer == nil {
		return fmt.Errorf("events: nil producer")
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}
	msg := kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: body,
		Headers: []kafka.Header{
			{Key: "ce-id", Value: []byte(env.ID)},
			{Key: "ce-type", Value: []byte(env.Type)},
			{Key: "ce-source", Value: []byte(env.Source)},
			{Key: "ce-specversion", Value: []byte(env.SpecVersion)},
		},
	}
	if env.CorrelationID != "" {
		msg.Headers = append(msg.Headers, kafka.Header{Key: "ce-correlationid", Value: []byte(env.CorrelationID)})
	}
	if env.CausationID != "" {
		msg.Headers = append(msg.Headers, kafka.Header{Key: "ce-causationid", Value: []byte(env.CausationID)})
	}
	return p.writer.WriteMessages(ctx, msg)
}

// Close shuts down the underlying writer.
func (p *Producer) Close() error {
	if p == nil || p.writer == nil {
		return nil
	}
	return p.writer.Close()
}
