// Package models holds the federation domain types.
//
// The federation gateway is the central authority for records that are edited
// in more than one place: edge nodes (depot nodes, POS terminals, field apps)
// capture and mutate records while offline, then push them here. Because two
// nodes can edit the same record during the same offline window, "sync" is not
// a copy — it is a merge with an explicit conflict policy.
//
// The model is deliberately generic. The gateway does not know what a delivery
// note or a stock count is; it stores an opaque JSON payload keyed by
// (ResourceType, ResourceID) and arbitrates *ordering* and *conflicts* over it.
// Domain services stay the owners of meaning; the gateway owns convergence.
package models

import (
	"encoding/json"
	"strings"
	"time"
)

// NodeStatus is the lifecycle state of a registered edge node.
type NodeStatus string

const (
	NodeActive   NodeStatus = "active"
	NodeInactive NodeStatus = "inactive"
	// NodeSuspended stops a node from pushing without deleting its history —
	// used when a node is known-bad (clock skew, corrupt buffer) and must be
	// quarantined while its already-applied changes stay valid.
	NodeSuspended NodeStatus = "suspended"
)

// Node is a federated edge deployment that syncs through this gateway.
type Node struct {
	ID           string     `json:"id"`
	NodeID       string     `json:"nodeId"` // stable logical id chosen by the node (e.g. depot-kla-01)
	Name         string     `json:"name"`
	Kind         string     `json:"kind"` // depot-node | pos-terminal | field-app | ...
	Status       NodeStatus `json:"status"`
	LastSeenAt   *time.Time `json:"lastSeenAt,omitempty"`
	LastCursor   int64      `json:"lastCursor"` // highest change cursor this node has acknowledged
	RegisteredAt time.Time  `json:"registeredAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// CanPush reports whether the node is permitted to submit changes.
func (n Node) CanPush() bool { return n.Status == NodeActive }

// Resource is the authoritative central copy of a federated record.
type Resource struct {
	ResourceType  string          `json:"resourceType"`
	ResourceID    string          `json:"resourceId"`
	Revision      int64           `json:"revision"` // bumped on every applied change
	Payload       json.RawMessage `json:"payload"`
	Deleted       bool            `json:"deleted"`
	UpdatedAt     time.Time       `json:"updatedAt"`     // when the gateway applied it
	OriginNodeID  string          `json:"originNodeId"`  // node whose change produced this revision
	OriginStampAt time.Time       `json:"originStampAt"` // the node's own edit timestamp (used by last-write-wins)
}

// Key is the composite identity of a federated record.
func (r Resource) Key() string { return r.ResourceType + "/" + r.ResourceID }

// ChangeOp is the kind of mutation a node is pushing.
type ChangeOp string

const (
	OpUpsert ChangeOp = "upsert"
	OpDelete ChangeOp = "delete"
)

// Valid reports whether the op is one the gateway understands.
func (o ChangeOp) Valid() bool { return o == OpUpsert || o == OpDelete }

// Change is one mutation submitted by a node.
//
// BaseRevision is the crux of conflict detection: it is the revision the node
// believed it was editing. If it does not match the current central revision,
// someone else changed the record while this node was offline.
type Change struct {
	ChangeID     string          `json:"changeId"` // node-generated uuid; makes push idempotent
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Op           ChangeOp        `json:"op"`
	BaseRevision int64           `json:"baseRevision"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	// UpdatedAt is the node's own wall-clock time for the edit. Used only by
	// the last-write-wins strategy, and never trusted for ordering the log —
	// edge clocks drift, so the authoritative order is the server cursor.
	UpdatedAt time.Time `json:"updatedAt"`
}

// Normalize trims and lowercases the identity fields so that "Invoice" and
// "invoice" cannot become two divergent records that never converge.
func (c *Change) Normalize() {
	c.ResourceType = strings.ToLower(strings.TrimSpace(c.ResourceType))
	c.ResourceID = strings.TrimSpace(c.ResourceID)
	c.ChangeID = strings.TrimSpace(c.ChangeID)
}

// ChangeOutcome is the per-item result the gateway returns for a pushed change.
type ChangeOutcome string

const (
	// OutcomeApplied means the change is now the authoritative revision.
	OutcomeApplied ChangeOutcome = "applied"
	// OutcomeDuplicate means this change_id was already applied — a replay.
	OutcomeDuplicate ChangeOutcome = "duplicate"
	// OutcomeConflictResolved means a conflict was detected and settled
	// automatically by the configured strategy.
	OutcomeConflictResolved ChangeOutcome = "conflict_resolved"
	// OutcomeConflictPending means a conflict was detected and parked for a
	// human. The central record is unchanged.
	OutcomeConflictPending ChangeOutcome = "conflict_pending"
	// OutcomeRejected means the change was refused (stale under server-wins,
	// suspended node, invalid payload).
	OutcomeRejected ChangeOutcome = "rejected"
)

// PushResult is the outcome for a single submitted change.
type PushResult struct {
	ChangeID   string        `json:"changeId"`
	Outcome    ChangeOutcome `json:"outcome"`
	Revision   int64         `json:"revision,omitempty"`   // resulting central revision
	Cursor     int64         `json:"cursor,omitempty"`     // log position, for the node's watermark
	ConflictID string        `json:"conflictId,omitempty"` // set when a conflict was recorded
	Reason     string        `json:"reason,omitempty"`
}

// LogEntry is one applied change in the append-only federation log. Nodes pull
// deltas by cursor; the cursor is a gateway-assigned monotonic sequence, which
// is why edge clock skew cannot corrupt replication order.
type LogEntry struct {
	Cursor       int64           `json:"cursor"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Op           ChangeOp        `json:"op"`
	Revision     int64           `json:"revision"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	OriginNodeID string          `json:"originNodeId"`
	AppliedAt    time.Time       `json:"appliedAt"`
}

// ConflictState is the lifecycle of a parked conflict.
type ConflictState string

const (
	ConflictPending  ConflictState = "pending"
	ConflictResolved ConflictState = "resolved"
)

// Resolution is how a parked conflict was settled.
type Resolution string

const (
	ResolveKeepServer Resolution = "keep_server" // discard the node's change
	ResolveKeepNode   Resolution = "keep_node"   // apply the node's change as-is
	ResolveMerged     Resolution = "merged"      // apply an operator-supplied merged payload
)

// Valid reports whether the resolution is one the gateway can apply.
func (r Resolution) Valid() bool {
	return r == ResolveKeepServer || r == ResolveKeepNode || r == ResolveMerged
}

// Conflict is a divergence parked for manual resolution.
type Conflict struct {
	ID             string          `json:"id"`
	ResourceType   string          `json:"resourceType"`
	ResourceID     string          `json:"resourceId"`
	NodeID         string          `json:"nodeId"`
	ChangeID       string          `json:"changeId"`
	Op             ChangeOp        `json:"op"`
	BaseRevision   int64           `json:"baseRevision"`
	ServerRevision int64           `json:"serverRevision"`
	NodePayload    json.RawMessage `json:"nodePayload,omitempty"`
	ServerPayload  json.RawMessage `json:"serverPayload,omitempty"`
	State          ConflictState   `json:"state"`
	Resolution     *Resolution     `json:"resolution,omitempty"`
	ResolvedBy     string          `json:"resolvedBy,omitempty"`
	ResolvedAt     *time.Time      `json:"resolvedAt,omitempty"`
	DetectedAt     time.Time       `json:"detectedAt"`
}

// Strategy is the automatic conflict-resolution policy.
type Strategy string

const (
	// StrategyLastWriteWins compares the node's edit stamp against the stamp
	// that produced the current central revision; the later edit wins.
	StrategyLastWriteWins Strategy = "last_write_wins"
	// StrategyServerWins always keeps the central record and rejects the node's
	// change. Safest for records the centre owns.
	StrategyServerWins Strategy = "server_wins"
	// StrategyNodeWins always applies the node's change. Appropriate when the
	// edge is the system of record (e.g. physical stock counts).
	StrategyNodeWins Strategy = "node_wins"
	// StrategyManual parks every conflict for a human. Nothing is lost and
	// nothing is guessed, at the cost of a queue that must be worked.
	StrategyManual Strategy = "manual"
)

// Valid reports whether the strategy is recognised.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyLastWriteWins, StrategyServerWins, StrategyNodeWins, StrategyManual:
		return true
	}
	return false
}

// SyncStats summarises gateway state for the node/status endpoints.
type SyncStats struct {
	Nodes             int   `json:"nodes"`
	ActiveNodes       int   `json:"activeNodes"`
	Resources         int   `json:"resources"`
	PendingConflicts  int   `json:"pendingConflicts"`
	ResolvedConflicts int   `json:"resolvedConflicts"`
	LatestCursor      int64 `json:"latestCursor"`
}
