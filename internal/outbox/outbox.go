// Package outbox implements the transactional outbox for federation events.
//
// EnqueueTx is the reason this exists rather than publishing to Kafka inline:
// an applied change and the event announcing it must commit together. Publish
// then commit can announce a change that later rolls back; commit then publish
// can lose the announcement entirely if the process dies in between.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iag/federation-gateway/internal/events"
)

// ErrNotEnqueued is returned when the store is not backed by a pool.
var ErrNotEnqueued = errors.New("outbox: not enqueued")

// Row is a pending or completed outbox entry.
type Row struct {
	ID        int64
	EventType string
	EventKey  string
	Payload   json.RawMessage
	Attempts  int
}

// Store wraps the federation_event_outbox table.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Enqueue writes an event outside any caller transaction.
func (s *Store) Enqueue(ctx context.Context, eventType, key string, payload any) error {
	if s == nil || s.pool == nil {
		return ErrNotEnqueued
	}
	body, err := marshalEnvelope(eventType, payload)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO federation_event_outbox (event_type, event_key, payload)
		VALUES ($1, $2, $3::jsonb)`, eventType, nullable(key), body)
	return err
}

// EnqueueTx writes an event inside the caller's transaction, so it commits or
// rolls back with the change it describes.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, eventType, key string, payload any) error {
	body, err := marshalEnvelope(eventType, payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO federation_event_outbox (event_type, event_key, payload)
		VALUES ($1, $2, $3::jsonb)`, eventType, nullable(key), body)
	return err
}

// marshalEnvelope wraps the payload in a CloudEvents envelope so the drained
// row is publishable as-is.
func marshalEnvelope(eventType string, payload any) ([]byte, error) {
	env, err := events.NewEnvelope(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("build outbox envelope: %w", err)
	}
	return json.Marshal(env)
}

func (s *Store) ClaimBatch(ctx context.Context, limit int, backoff time.Duration) ([]Row, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM federation_event_outbox
			WHERE dispatched_at IS NULL AND available_at <= NOW()
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE federation_event_outbox o
		SET attempts = o.attempts + 1,
		    available_at = NOW() + $2::interval
		FROM due
		WHERE o.id = due.id
		RETURNING o.id, o.event_type, COALESCE(o.event_key, ''), o.payload, o.attempts
	`, limit, fmt.Sprintf("%d milliseconds", backoff.Milliseconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.EventType, &r.EventKey, &r.Payload, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkDispatched(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE federation_event_outbox SET dispatched_at = NOW(), last_error = NULL WHERE id = $1`, id)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, id int64, errMsg string, retryDelay time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE federation_event_outbox
		SET last_error = $1, available_at = NOW() + $2::interval
		WHERE id = $3`, errMsg, fmt.Sprintf("%d milliseconds", retryDelay.Milliseconds()), id)
	return err
}

// Dispatcher writes a drained outbox row to Kafka.
type Dispatcher interface {
	DispatchOutbox(ctx context.Context, eventType, eventKey string, payload []byte) error
}

// Publisher periodically drains the outbox.
type Publisher struct {
	store      *Store
	dispatcher Dispatcher
	tick       time.Duration
	batch      int
	maxBackoff time.Duration
}

func NewPublisher(store *Store, d Dispatcher) *Publisher {
	return &Publisher{store: store, dispatcher: d, tick: 2 * time.Second, batch: 32, maxBackoff: 5 * time.Minute}
}

func (p *Publisher) Run(ctx context.Context) {
	if p == nil || p.store == nil || p.dispatcher == nil {
		return
	}
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := p.drainOnce(ctx)
			if err != nil {
				slog.Warn("outbox drain", "err", err)
				continue
			}
			if n >= p.batch {
				_, _ = p.drainOnce(ctx)
			}
		}
	}
}

func (p *Publisher) drainOnce(ctx context.Context) (int, error) {
	rows, err := p.store.ClaimBatch(ctx, p.batch, time.Second)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := p.dispatcher.DispatchOutbox(ctx, r.EventType, r.EventKey, r.Payload); err != nil {
			_ = p.store.MarkFailed(ctx, r.ID, err.Error(), backoffFor(r.Attempts, p.maxBackoff))
			slog.Warn("outbox dispatch failed", "id", r.ID, "type", r.EventType, "err", err)
			continue
		}
		_ = p.store.MarkDispatched(ctx, r.ID)
	}
	return len(rows), nil
}

func backoffFor(attempts int, max time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Duration(math.Pow(2, float64(attempts))) * time.Second
	if d > max {
		return max
	}
	return d
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
