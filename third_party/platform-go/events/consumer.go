package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// Handler processes one event. Returning a permanent error sends the message
// to the DLQ; returning a transient error causes a retry with backoff.
type Handler interface {
	Handle(ctx context.Context, env Envelope) error
}

// HandlerFunc adapts a function into a Handler.
type HandlerFunc func(ctx context.Context, env Envelope) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, env Envelope) error { return f(ctx, env) }

// PermanentError marks a handler error as non-retryable; the consumer routes
// the offending message to the DLQ.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as non-retryable.
func Permanent(err error) error { return &PermanentError{Err: err} }

// Dedupe records which event IDs have been processed and short-circuits
// re-deliveries. Implementations must be safe for concurrent use.
type Dedupe interface {
	// Seen returns true if eventID has already been processed.
	Seen(ctx context.Context, eventID string) (bool, error)
	// Mark records eventID as processed.
	Mark(ctx context.Context, eventID string) error
}

// NoopDedupe is a Dedupe that never reports duplicates. Useful for tests or
// for stateless handlers that are themselves idempotent.
type NoopDedupe struct{}

// Seen always returns false.
func (NoopDedupe) Seen(context.Context, string) (bool, error) { return false, nil }

// Mark is a no-op.
func (NoopDedupe) Mark(context.Context, string) error { return nil }

// ConsumerConfig configures a Consumer.
type ConsumerConfig struct {
	Brokers []string
	Topic   string
	GroupID string

	Handler Handler
	Dedupe  Dedupe // optional — defaults to NoopDedupe

	// DLQ destination. If DLQProducer is nil, poison messages are logged and
	// committed (skipped) without DLQ delivery.
	DLQProducer *Producer
	DLQTopic    string

	// MaxRetries controls how many times a transient handler error is retried
	// before the message is treated as poison. Defaults to 3.
	MaxRetries int
	// RetryBackoff is the initial delay between retries; doubles each attempt.
	RetryBackoff time.Duration
}

// Consumer reads from Kafka, deduplicates by event id, retries transient
// failures with backoff, and routes permanent failures to a DLQ.
type Consumer struct {
	cfg    ConsumerConfig
	reader *kafka.Reader
}

// NewConsumer constructs a Consumer.
func NewConsumer(cfg ConsumerConfig) (*Consumer, error) {
	if cfg.Handler == nil {
		return nil, errors.New("events: ConsumerConfig.Handler is required")
	}
	if cfg.Topic == "" || cfg.GroupID == "" || len(cfg.Brokers) == 0 {
		return nil, errors.New("events: ConsumerConfig requires Brokers, Topic, GroupID")
	}
	if cfg.Dedupe == nil {
		cfg.Dedupe = NoopDedupe{}
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = 500 * time.Millisecond
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        cfg.Brokers,
		Topic:          cfg.Topic,
		GroupID:        cfg.GroupID,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		StartOffset:    kafka.LastOffset,
		CommitInterval: 0,
		Logger:         kafka.LoggerFunc(func(msg string, args ...any) { slog.Debug(msg, "args", args) }),
		ErrorLogger:    kafka.LoggerFunc(func(msg string, args ...any) { slog.Warn(msg, "args", args) }),
	})
	return &Consumer{cfg: cfg, reader: reader}, nil
}

// Run blocks consuming until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	slog.Info("events consumer started", "topic", c.cfg.Topic, "group", c.cfg.GroupID, "dlq", c.cfg.DLQTopic)
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			slog.Warn("events fetch failed", "error", err)
			continue
		}

		var env Envelope
		if err := json.Unmarshal(msg.Value, &env); err != nil {
			c.toDLQ(ctx, msg, fmt.Errorf("decode envelope: %w", err))
			c.commit(ctx, msg)
			continue
		}

		seen, err := c.cfg.Dedupe.Seen(ctx, env.ID)
		if err != nil {
			slog.Warn("dedupe lookup failed; processing anyway", "event_id", env.ID, "error", err)
		}
		if seen {
			slog.Debug("event already processed; skipping", "event_id", env.ID, "type", env.Type)
			c.commit(ctx, msg)
			continue
		}

		if err := c.process(ctx, env); err != nil {
			c.toDLQ(ctx, msg, err)
			c.commit(ctx, msg)
			continue
		}

		if err := c.cfg.Dedupe.Mark(ctx, env.ID); err != nil {
			slog.Warn("dedupe mark failed", "event_id", env.ID, "error", err)
		}
		c.commit(ctx, msg)
	}
}

func (c *Consumer) process(ctx context.Context, env Envelope) error {
	backoff := c.cfg.RetryBackoff
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		err := c.cfg.Handler.Handle(ctx, env)
		if err == nil {
			return nil
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			return err
		}
		lastErr = err
		slog.Warn("event handler transient failure",
			"event_id", env.ID, "type", env.Type, "attempt", attempt+1, "error", err)
	}
	return fmt.Errorf("handler failed after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

func (c *Consumer) commit(ctx context.Context, msg kafka.Message) {
	if err := c.reader.CommitMessages(ctx, msg); err != nil {
		slog.Warn("events commit failed", "error", err, "offset", msg.Offset)
	}
}

func (c *Consumer) toDLQ(ctx context.Context, msg kafka.Message, cause error) {
	slog.Error("events routing to DLQ",
		"topic", c.cfg.Topic, "offset", msg.Offset, "dlq", c.cfg.DLQTopic, "error", cause)
	if c.cfg.DLQProducer == nil || c.cfg.DLQTopic == "" {
		return
	}
	dlqEnv := NewEnvelope("iag.platform-go", "dlq.message", map[string]any{
		"sourceTopic":  c.cfg.Topic,
		"sourceOffset": msg.Offset,
		"error":        cause.Error(),
		"payload":      string(msg.Value),
	})
	if err := c.cfg.DLQProducer.Publish(ctx, c.cfg.DLQTopic, string(msg.Key), dlqEnv); err != nil {
		slog.Error("DLQ publish failed", "error", err)
	}
}

// Close shuts down the underlying reader.
func (c *Consumer) Close() error {
	if c == nil || c.reader == nil {
		return nil
	}
	return c.reader.Close()
}
