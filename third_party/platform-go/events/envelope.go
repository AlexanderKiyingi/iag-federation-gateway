// Package events implements the IAG event bus runtime: Kafka producer with
// idempotent writes, and consumer with dedupe + DLQ.
package events

import (
	"time"

	"github.com/google/uuid"
)

// SpecVersion is the IAG event envelope version.
const SpecVersion = "1.0"

// Envelope is the CloudEvents-compatible IAG event payload. Every event on
// the bus carries this shape; the domain-specific data lives under Data.
type Envelope struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Time          string         `json:"time"`
	Source        string         `json:"source"`
	SpecVersion   string         `json:"specversion"`
	CorrelationID string         `json:"correlationId,omitempty"`
	CausationID   string         `json:"causationId,omitempty"`
	Data          map[string]any `json:"data"`
}

// NewEnvelope returns an envelope with id, time, and specversion populated.
func NewEnvelope(source, eventType string, data map[string]any) Envelope {
	return Envelope{
		ID:          uuid.NewString(),
		Type:        eventType,
		Time:        time.Now().UTC().Format(time.RFC3339Nano),
		Source:      source,
		SpecVersion: SpecVersion,
		Data:        data,
	}
}
