// Package store persists federation state in Postgres.
//
// The gateway has no in-memory mode: it is the authoritative record of what
// every edge node has synced, and losing that on restart would silently
// re-apply or drop changes. A durable store is a correctness requirement here,
// not a deployment preference.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iag/federation-gateway/internal/events"
	"github.com/iag/federation-gateway/internal/federation"
	"github.com/iag/federation-gateway/internal/models"
)

// Event types emitted by the store, aliased so call sites read locally while
// the canonical names stay owned by the events package.
const (
	EventChangeApplied    = events.TypeChangeApplied
	EventConflictDetected = events.TypeConflictDetected
	EventConflictResolved = events.TypeConflictResolved
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrNodeNotAllowed is returned when a suspended/unknown node attempts a push.
var ErrNodeNotAllowed = errors.New("node not permitted to sync")

// Store is the Postgres-backed federation store.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Ping supports the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ---------------------------------------------------------------- nodes

// RegisterNode upserts a node and stamps it as seen. Registration is
// idempotent: a node calls this on every boot and heartbeat.
//
// Status is deliberately not reset on re-registration — a suspended node must
// not be able to un-suspend itself simply by restarting.
func (s *Store) RegisterNode(ctx context.Context, n models.Node) (models.Node, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO federation_nodes (node_id, name, kind, last_seen_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (node_id) DO UPDATE
		SET name         = COALESCE(NULLIF(EXCLUDED.name, ''), federation_nodes.name),
		    kind         = COALESCE(NULLIF(EXCLUDED.kind, 'unknown'), federation_nodes.kind),
		    last_seen_at = NOW(),
		    updated_at   = NOW()
		RETURNING id, node_id, name, kind, status, last_seen_at, last_cursor, registered_at, updated_at`,
		n.NodeID, n.Name, defaultKind(n.Kind))
	return scanNode(row)
}

func defaultKind(k string) string {
	if k == "" {
		return "unknown"
	}
	return k
}

func (s *Store) GetNode(ctx context.Context, nodeID string) (models.Node, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, node_id, name, kind, status, last_seen_at, last_cursor, registered_at, updated_at
		FROM federation_nodes WHERE node_id = $1`, nodeID)
	n, err := scanNode(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Node{}, ErrNotFound
	}
	return n, err
}

func (s *Store) ListNodes(ctx context.Context) ([]models.Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, node_id, name, kind, status, last_seen_at, last_cursor, registered_at, updated_at
		FROM federation_nodes ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetNodeStatus suspends or reactivates a node.
func (s *Store) SetNodeStatus(ctx context.Context, nodeID string, status models.NodeStatus) (models.Node, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE federation_nodes SET status = $2, updated_at = NOW()
		WHERE node_id = $1
		RETURNING id, node_id, name, kind, status, last_seen_at, last_cursor, registered_at, updated_at`,
		nodeID, string(status))
	n, err := scanNode(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Node{}, ErrNotFound
	}
	return n, err
}

// AckCursor records how far a node has consumed the log. It only ever moves
// forward: a node replaying an old pull must not rewind its own watermark.
func (s *Store) AckCursor(ctx context.Context, nodeID string, cursor int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE federation_nodes
		SET last_cursor = GREATEST(last_cursor, $2), last_seen_at = NOW(), updated_at = NOW()
		WHERE node_id = $1`, nodeID, cursor)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(r rowScanner) (models.Node, error) {
	var n models.Node
	var status string
	err := r.Scan(&n.ID, &n.NodeID, &n.Name, &n.Kind, &status,
		&n.LastSeenAt, &n.LastCursor, &n.RegisteredAt, &n.UpdatedAt)
	n.Status = models.NodeStatus(status)
	return n, err
}

// ---------------------------------------------------------------- resources

func (s *Store) GetResource(ctx context.Context, resourceType, resourceID string) (models.Resource, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT resource_type, resource_id, revision, payload, deleted,
		       origin_node_id, origin_stamp_at, updated_at
		FROM federation_resources WHERE resource_type = $1 AND resource_id = $2`,
		resourceType, resourceID)
	var r models.Resource
	err := row.Scan(&r.ResourceType, &r.ResourceID, &r.Revision, &r.Payload, &r.Deleted,
		&r.OriginNodeID, &r.OriginStampAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Resource{}, ErrNotFound
	}
	return r, err
}

// ---------------------------------------------------------------- push

// eventSink receives events enqueued inside the same transaction as the change
// they describe, so an applied change can never fail to announce itself.
type eventSink interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, eventType, key string, payload any) error
}

// ApplyPush arbitrates and applies a batch of changes from one node.
//
// Each change runs in its own transaction. A push is a batch for transport
// efficiency, not an atomic unit: partial success is the expected outcome when
// some rows conflict, and one parked conflict must not roll back the changes
// that merged cleanly. Per-change results tell the node exactly what happened.
func (s *Store) ApplyPush(
	ctx context.Context,
	nodeID string,
	changes []models.Change,
	strategy models.Strategy,
	events eventSink,
) ([]models.PushResult, error) {
	node, err := s.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if !node.CanPush() {
		return nil, fmt.Errorf("%w: node %s is %s", ErrNodeNotAllowed, nodeID, node.Status)
	}

	results := make([]models.PushResult, 0, len(changes))
	for _, ch := range changes {
		ch.Normalize()
		res, err := s.applyOne(ctx, nodeID, ch, strategy, events)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

func (s *Store) applyOne(
	ctx context.Context,
	nodeID string,
	ch models.Change,
	strategy models.Strategy,
	events eventSink,
) (models.PushResult, error) {
	if ch.ChangeID == "" || ch.ResourceType == "" || ch.ResourceID == "" {
		return models.PushResult{
			ChangeID: ch.ChangeID, Outcome: models.OutcomeRejected,
			Reason: "changeId, resourceType and resourceId are required",
		}, nil
	}
	if !ch.Op.Valid() {
		return models.PushResult{
			ChangeID: ch.ChangeID, Outcome: models.OutcomeRejected,
			Reason: "op must be upsert or delete",
		}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return models.PushResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency first: a node that never saw our ack will resend.
	if prior, ok, err := priorOutcome(ctx, tx, nodeID, ch.ChangeID); err != nil {
		return models.PushResult{}, err
	} else if ok {
		prior.Outcome = models.OutcomeDuplicate
		return prior, nil
	}

	// Lock the resource row so two nodes pushing the same key serialise.
	current, exists, err := lockResource(ctx, tx, ch.ResourceType, ch.ResourceID)
	if err != nil {
		return models.PushResult{}, err
	}

	decision := federation.Decide(nodeID, ch, current, exists, strategy)
	result := models.PushResult{ChangeID: ch.ChangeID, Outcome: decision.Outcome(), Reason: decision.Reason}

	switch decision.Verdict {
	case federation.Apply:
		rev, cursor, err := applyChange(ctx, tx, nodeID, ch, current, exists)
		if err != nil {
			return models.PushResult{}, err
		}
		result.Revision, result.Cursor = rev, cursor
		if events != nil {
			_ = events.EnqueueTx(ctx, tx, EventChangeApplied, ch.ResourceType+"/"+ch.ResourceID, map[string]any{
				"resourceType": ch.ResourceType,
				"resourceId":   ch.ResourceID,
				"op":           string(ch.Op),
				"revision":     rev,
				"cursor":       cursor,
				"nodeId":       nodeID,
				"conflicted":   decision.Conflicted,
			})
		}

	case federation.Park:
		conflictID, err := recordConflict(ctx, tx, nodeID, ch, current)
		if err != nil {
			return models.PushResult{}, err
		}
		result.ConflictID = conflictID
		result.Revision = current.Revision
		if events != nil {
			_ = events.EnqueueTx(ctx, tx, EventConflictDetected, ch.ResourceType+"/"+ch.ResourceID, map[string]any{
				"conflictId":   conflictID,
				"resourceType": ch.ResourceType,
				"resourceId":   ch.ResourceID,
				"nodeId":       nodeID,
				"baseRevision": ch.BaseRevision,
				"serverRevision": current.Revision,
			})
		}

	case federation.Discard:
		result.Revision = current.Revision
	}

	if err := recordChange(ctx, tx, nodeID, ch.ChangeID, result); err != nil {
		return models.PushResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return models.PushResult{}, err
	}
	return result, nil
}

func priorOutcome(ctx context.Context, tx pgx.Tx, nodeID, changeID string) (models.PushResult, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT outcome, COALESCE(revision, 0), COALESCE(cursor, 0), COALESCE(conflict_id::text, '')
		FROM federation_changes WHERE node_id = $1 AND change_id = $2`, nodeID, changeID)
	var r models.PushResult
	var outcome string
	err := row.Scan(&outcome, &r.Revision, &r.Cursor, &r.ConflictID)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.PushResult{}, false, nil
	}
	if err != nil {
		return models.PushResult{}, false, err
	}
	r.ChangeID = changeID
	r.Reason = "already applied as " + outcome
	return r, true, nil
}

func lockResource(ctx context.Context, tx pgx.Tx, resourceType, resourceID string) (models.Resource, bool, error) {
	row := tx.QueryRow(ctx, `
		SELECT resource_type, resource_id, revision, payload, deleted,
		       origin_node_id, origin_stamp_at, updated_at
		FROM federation_resources
		WHERE resource_type = $1 AND resource_id = $2
		FOR UPDATE`, resourceType, resourceID)
	var r models.Resource
	err := row.Scan(&r.ResourceType, &r.ResourceID, &r.Revision, &r.Payload, &r.Deleted,
		&r.OriginNodeID, &r.OriginStampAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Resource{}, false, nil
	}
	if err != nil {
		return models.Resource{}, false, err
	}
	return r, true, nil
}

// applyChange writes the new authoritative revision and appends to the log.
func applyChange(
	ctx context.Context, tx pgx.Tx, nodeID string,
	ch models.Change, current models.Resource, exists bool,
) (revision int64, cursor int64, err error) {
	payload := ch.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	deleted := ch.Op == models.OpDelete
	stamp := ch.UpdatedAt
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}

	if exists {
		revision = current.Revision + 1
		_, err = tx.Exec(ctx, `
			UPDATE federation_resources
			SET revision = $3, payload = $4::jsonb, deleted = $5,
			    origin_node_id = $6, origin_stamp_at = $7, updated_at = NOW()
			WHERE resource_type = $1 AND resource_id = $2`,
			ch.ResourceType, ch.ResourceID, revision, payload, deleted, nodeID, stamp)
	} else {
		revision = 1
		_, err = tx.Exec(ctx, `
			INSERT INTO federation_resources
			    (resource_type, resource_id, revision, payload, deleted, origin_node_id, origin_stamp_at)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)`,
			ch.ResourceType, ch.ResourceID, revision, payload, deleted, nodeID, stamp)
	}
	if err != nil {
		return 0, 0, err
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO federation_log (resource_type, resource_id, op, revision, payload, origin_node_id)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		RETURNING cursor`,
		ch.ResourceType, ch.ResourceID, string(ch.Op), revision, payload, nodeID).Scan(&cursor)
	return revision, cursor, err
}

func recordConflict(ctx context.Context, tx pgx.Tx, nodeID string, ch models.Change, current models.Resource) (string, error) {
	var id string
	serverPayload := current.Payload
	if len(serverPayload) == 0 {
		serverPayload = json.RawMessage(`{}`)
	}
	nodePayload := ch.Payload
	if len(nodePayload) == 0 {
		nodePayload = json.RawMessage(`{}`)
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO federation_conflicts
		    (resource_type, resource_id, node_id, change_id, op,
		     base_revision, server_revision, node_payload, server_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
		RETURNING id`,
		ch.ResourceType, ch.ResourceID, nodeID, ch.ChangeID, string(ch.Op),
		ch.BaseRevision, current.Revision, nodePayload, serverPayload).Scan(&id)
	return id, err
}

func recordChange(ctx context.Context, tx pgx.Tx, nodeID, changeID string, r models.PushResult) error {
	var conflictID any
	if r.ConflictID != "" {
		conflictID = r.ConflictID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO federation_changes (node_id, change_id, outcome, revision, cursor, conflict_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (node_id, change_id) DO NOTHING`,
		nodeID, changeID, string(r.Outcome), nullableInt(r.Revision), nullableInt(r.Cursor), conflictID)
	return err
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ---------------------------------------------------------------- pull

// Pull returns log entries after the given cursor.
//
// excludeNode omits entries the caller itself authored — a node has no use for
// its own writes echoed back, and replaying them would make it re-apply edits
// it already holds.
func (s *Store) Pull(ctx context.Context, after int64, limit int, excludeNode string) ([]models.LogEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT cursor, resource_type, resource_id, op, revision, payload, origin_node_id, applied_at
		FROM federation_log
		WHERE cursor > $1 AND ($3 = '' OR origin_node_id <> $3)
		ORDER BY cursor
		LIMIT $2`, after, limit, excludeNode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.LogEntry{}
	for rows.Next() {
		var e models.LogEntry
		var op string
		if err := rows.Scan(&e.Cursor, &e.ResourceType, &e.ResourceID, &op,
			&e.Revision, &e.Payload, &e.OriginNodeID, &e.AppliedAt); err != nil {
			return nil, err
		}
		e.Op = models.ChangeOp(op)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- conflicts

func (s *Store) ListConflicts(ctx context.Context, state string, limit int) ([]models.Conflict, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, resource_type, resource_id, node_id, change_id, op,
		       base_revision, server_revision, node_payload, server_payload,
		       state, resolution, COALESCE(resolved_by, ''), resolved_at, detected_at
		FROM federation_conflicts
		WHERE ($1 = '' OR state = $1)
		ORDER BY detected_at DESC
		LIMIT $2`, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Conflict{}
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetConflict(ctx context.Context, id string) (models.Conflict, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, resource_type, resource_id, node_id, change_id, op,
		       base_revision, server_revision, node_payload, server_payload,
		       state, resolution, COALESCE(resolved_by, ''), resolved_at, detected_at
		FROM federation_conflicts WHERE id = $1`, id)
	c, err := scanConflict(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Conflict{}, ErrNotFound
	}
	return c, err
}

func scanConflict(r rowScanner) (models.Conflict, error) {
	var c models.Conflict
	var op, state string
	var resolution *string
	err := r.Scan(&c.ID, &c.ResourceType, &c.ResourceID, &c.NodeID, &c.ChangeID, &op,
		&c.BaseRevision, &c.ServerRevision, &c.NodePayload, &c.ServerPayload,
		&state, &resolution, &c.ResolvedBy, &c.ResolvedAt, &c.DetectedAt)
	c.Op = models.ChangeOp(op)
	c.State = models.ConflictState(state)
	if resolution != nil {
		res := models.Resolution(*resolution)
		c.Resolution = &res
	}
	return c, err
}

// ResolveConflict settles a parked conflict and, where the resolution calls for
// it, writes the winning payload as a new authoritative revision.
//
// The whole settlement is one transaction: a conflict marked resolved while its
// payload failed to land would leave the record permanently diverged with no
// queue entry left to notice it.
func (s *Store) ResolveConflict(
	ctx context.Context, id string, resolution models.Resolution,
	mergedPayload json.RawMessage, actor string, events eventSink,
) (models.Conflict, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return models.Conflict{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		SELECT id, resource_type, resource_id, node_id, change_id, op,
		       base_revision, server_revision, node_payload, server_payload,
		       state, resolution, COALESCE(resolved_by, ''), resolved_at, detected_at
		FROM federation_conflicts WHERE id = $1 FOR UPDATE`, id)
	c, err := scanConflict(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Conflict{}, ErrNotFound
	}
	if err != nil {
		return models.Conflict{}, err
	}
	if c.State == models.ConflictResolved {
		return c, fmt.Errorf("conflict %s is already resolved", id)
	}

	if resolution != models.ResolveKeepServer {
		payload := c.NodePayload
		if resolution == models.ResolveMerged {
			if len(mergedPayload) == 0 {
				return models.Conflict{}, errors.New("merged resolution requires a payload")
			}
			payload = mergedPayload
		}
		current, exists, err := lockResource(ctx, tx, c.ResourceType, c.ResourceID)
		if err != nil {
			return models.Conflict{}, err
		}
		ch := models.Change{
			ResourceType: c.ResourceType,
			ResourceID:   c.ResourceID,
			Op:           c.Op,
			Payload:      payload,
			UpdatedAt:    time.Now().UTC(),
		}
		if _, _, err := applyChange(ctx, tx, c.NodeID, ch, current, exists); err != nil {
			return models.Conflict{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE federation_conflicts
		SET state = 'resolved', resolution = $2, resolved_by = $3, resolved_at = NOW()
		WHERE id = $1`, id, string(resolution), actor); err != nil {
		return models.Conflict{}, err
	}

	if events != nil {
		_ = events.EnqueueTx(ctx, tx, EventConflictResolved, c.ResourceType+"/"+c.ResourceID, map[string]any{
			"conflictId":   c.ID,
			"resourceType": c.ResourceType,
			"resourceId":   c.ResourceID,
			"resolution":   string(resolution),
			"resolvedBy":   actor,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return models.Conflict{}, err
	}
	return s.GetConflict(ctx, id)
}

// ---------------------------------------------------------------- stats

func (s *Store) Stats(ctx context.Context) (models.SyncStats, error) {
	var st models.SyncStats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM federation_nodes),
		  (SELECT COUNT(*) FROM federation_nodes WHERE status = 'active'),
		  (SELECT COUNT(*) FROM federation_resources),
		  (SELECT COUNT(*) FROM federation_conflicts WHERE state = 'pending'),
		  (SELECT COUNT(*) FROM federation_conflicts WHERE state = 'resolved'),
		  (SELECT COALESCE(MAX(cursor), 0) FROM federation_log)`).
		Scan(&st.Nodes, &st.ActiveNodes, &st.Resources, &st.PendingConflicts, &st.ResolvedConflicts, &st.LatestCursor)
	return st, err
}
