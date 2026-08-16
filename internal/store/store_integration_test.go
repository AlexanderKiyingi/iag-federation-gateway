package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	fgdb "github.com/iag/federation-gateway/db"
	"github.com/iag/federation-gateway/internal/migrate"
	"github.com/iag/federation-gateway/internal/models"
	"github.com/iag/federation-gateway/internal/store"
)

// These tests exercise the real schema and the real queries. Unit tests cover
// the merge rules; only this can catch a malformed migration, a column that
// does not exist, or a CHECK constraint that rejects a row the code writes.
//
// Set TEST_DATABASE_URL to run them:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/federation_test?sslmode=disable go test ./internal/store/...
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping database integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := migrate.Up(ctx, pool, fgdb.Migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Start each test from a clean slate; the migration is applied once and
	// re-used, so only the data is reset.
	if _, err := pool.Exec(ctx, `
		TRUNCATE federation_conflicts, federation_changes, federation_log,
		         federation_resources, federation_nodes, federation_event_outbox
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// capturingSink records events the way the outbox would, without needing Kafka.
type capturingSink struct{ types []string }

func (c *capturingSink) EnqueueTx(ctx context.Context, tx pgx.Tx, eventType, key string, payload any) error {
	c.types = append(c.types, eventType)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO federation_event_outbox (event_type, event_key, payload)
		VALUES ($1, $2, $3::jsonb)`, eventType, key, body)
	return err
}

func upsert(changeID, resourceID string, base int64, stamp time.Time, body string) models.Change {
	return models.Change{
		ChangeID:     changeID,
		ResourceType: "delivery_note",
		ResourceID:   resourceID,
		Op:           models.OpUpsert,
		BaseRevision: base,
		UpdatedAt:    stamp,
		Payload:      json.RawMessage(body),
	}
}

func registerNode(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if _, err := s.RegisterNode(context.Background(), models.Node{NodeID: id, Kind: "depot-node"}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestMigrationAppliesAndIsIdempotent(t *testing.T) {
	pool := testPool(t)
	// A second Up() over an already-migrated database must be a no-op rather
	// than a checksum error or a duplicate-object failure.
	applied, err := migrate.Up(context.Background(), pool, fgdb.Migrations())
	if err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("re-run applied %v, want none", applied)
	}
}

func TestPushApplyPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	sink := &capturingSink{}

	now := time.Now().UTC()
	res, err := s.ApplyPush(ctx, "node-a",
		[]models.Change{upsert("c1", "dn-1", 0, now, `{"qty":10}`)},
		models.StrategyLastWriteWins, sink)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(res) != 1 || res[0].Outcome != models.OutcomeApplied {
		t.Fatalf("push result = %+v, want applied", res)
	}
	if res[0].Revision != 1 || res[0].Cursor == 0 {
		t.Fatalf("expected revision 1 and a non-zero cursor, got %+v", res[0])
	}

	got, err := s.GetResource(ctx, "delivery_note", "dn-1")
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	if got.Revision != 1 || got.OriginNodeID != "node-a" {
		t.Fatalf("resource = %+v", got)
	}

	// A second node pulls the delta.
	entries, err := s.Pull(ctx, 0, 100, "node-b")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(entries) != 1 || entries[0].ResourceID != "dn-1" {
		t.Fatalf("pull = %+v, want the one change", entries)
	}

	// The author must not receive its own write back.
	own, err := s.Pull(ctx, 0, 100, "node-a")
	if err != nil {
		t.Fatalf("pull own: %v", err)
	}
	if len(own) != 0 {
		t.Fatalf("node-a pulled its own writes: %+v", own)
	}
}

// A resend of an acknowledged change must not create a second revision.
func TestPushIsIdempotentOnChangeID(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	now := time.Now().UTC()
	ch := upsert("dup-1", "dn-2", 0, now, `{"qty":1}`)

	first, err := s.ApplyPush(ctx, "node-a", []models.Change{ch}, models.StrategyLastWriteWins, nil)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	second, err := s.ApplyPush(ctx, "node-a", []models.Change{ch}, models.StrategyLastWriteWins, nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second[0].Outcome != models.OutcomeDuplicate {
		t.Fatalf("replay outcome = %q, want duplicate", second[0].Outcome)
	}
	res, _ := s.GetResource(ctx, "delivery_note", "dn-2")
	if res.Revision != first[0].Revision {
		t.Fatalf("replay bumped revision to %d, want %d", res.Revision, first[0].Revision)
	}
}

func TestConflictIsParkedAndResolvable(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	registerNode(t, s, "node-b")
	sink := &capturingSink{}
	now := time.Now().UTC()

	if _, err := s.ApplyPush(ctx, "node-a",
		[]models.Change{upsert("a1", "dn-3", 0, now, `{"qty":10}`)},
		models.StrategyManual, sink); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// node-b edited the same record believing it was still new.
	out, err := s.ApplyPush(ctx, "node-b",
		[]models.Change{upsert("b1", "dn-3", 0, now.Add(time.Minute), `{"qty":99}`)},
		models.StrategyManual, sink)
	if err != nil {
		t.Fatalf("conflicting push: %v", err)
	}
	if out[0].Outcome != models.OutcomeConflictPending || out[0].ConflictID == "" {
		t.Fatalf("expected a parked conflict, got %+v", out[0])
	}

	pending, err := s.ListConflicts(ctx, "pending", 10)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending conflicts = %d, want 1", len(pending))
	}

	// The central record must be untouched while the conflict is unresolved.
	before, _ := s.GetResource(ctx, "delivery_note", "dn-3")
	if before.Revision != 1 {
		t.Fatalf("parked conflict changed the record: %+v", before)
	}

	resolved, err := s.ResolveConflict(ctx, out[0].ConflictID, models.ResolveKeepNode, nil, "tester", sink)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.State != models.ConflictResolved || resolved.Resolution == nil {
		t.Fatalf("resolved conflict = %+v", resolved)
	}

	after, _ := s.GetResource(ctx, "delivery_note", "dn-3")
	if after.Revision != 2 {
		t.Fatalf("keep_node should have written a new revision, got %d", after.Revision)
	}

	// Resolving twice must fail rather than write another revision.
	if _, err := s.ResolveConflict(ctx, out[0].ConflictID, models.ResolveKeepNode, nil, "tester", sink); err == nil {
		t.Fatal("expected the second resolve to be rejected")
	}
}

func TestResolveKeepServerLeavesRecordUnchanged(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	registerNode(t, s, "node-b")
	now := time.Now().UTC()

	_, _ = s.ApplyPush(ctx, "node-a", []models.Change{upsert("a1", "dn-4", 0, now, `{"v":1}`)}, models.StrategyManual, nil)
	out, _ := s.ApplyPush(ctx, "node-b", []models.Change{upsert("b1", "dn-4", 0, now, `{"v":2}`)}, models.StrategyManual, nil)

	if _, err := s.ResolveConflict(ctx, out[0].ConflictID, models.ResolveKeepServer, nil, "tester", nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	res, _ := s.GetResource(ctx, "delivery_note", "dn-4")
	if res.Revision != 1 {
		t.Fatalf("keep_server must not write a revision, got %d", res.Revision)
	}
}

// The outbox row must be written in the same transaction as the change.
func TestEventsAreEnqueuedTransactionally(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	s := store.New(pool)
	registerNode(t, s, "node-a")
	sink := &capturingSink{}

	if _, err := s.ApplyPush(ctx, "node-a",
		[]models.Change{upsert("c1", "dn-5", 0, time.Now().UTC(), `{}`)},
		models.StrategyLastWriteWins, sink); err != nil {
		t.Fatalf("push: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM federation_event_outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

func TestSuspendedNodeCannotPush(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	if _, err := s.SetNodeStatus(ctx, "node-a", models.NodeSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, err := s.ApplyPush(ctx, "node-a",
		[]models.Change{upsert("c1", "dn-6", 0, time.Now().UTC(), `{}`)},
		models.StrategyLastWriteWins, nil)
	if err == nil {
		t.Fatal("suspended node was allowed to push")
	}

	// Re-registering must not clear the suspension.
	registerNode(t, s, "node-a")
	node, _ := s.GetNode(ctx, "node-a")
	if node.Status != models.NodeSuspended {
		t.Fatalf("re-registration un-suspended the node: %s", node.Status)
	}
}

func TestAckCursorOnlyMovesForward(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	if err := s.AckCursor(ctx, "node-a", 10); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := s.AckCursor(ctx, "node-a", 4); err != nil {
		t.Fatalf("ack rewind: %v", err)
	}
	node, _ := s.GetNode(ctx, "node-a")
	if node.LastCursor != 10 {
		t.Fatalf("cursor rewound to %d, want 10", node.LastCursor)
	}
}

func TestDeleteMarksResourceDeleted(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	now := time.Now().UTC()

	_, _ = s.ApplyPush(ctx, "node-a", []models.Change{upsert("c1", "dn-7", 0, now, `{"v":1}`)}, models.StrategyLastWriteWins, nil)
	del := models.Change{
		ChangeID: "c2", ResourceType: "delivery_note", ResourceID: "dn-7",
		Op: models.OpDelete, BaseRevision: 1, UpdatedAt: now.Add(time.Minute),
	}
	out, err := s.ApplyPush(ctx, "node-a", []models.Change{del}, models.StrategyLastWriteWins, nil)
	if err != nil {
		t.Fatalf("delete push: %v", err)
	}
	if out[0].Outcome != models.OutcomeApplied {
		t.Fatalf("delete outcome = %q", out[0].Outcome)
	}
	res, _ := s.GetResource(ctx, "delivery_note", "dn-7")
	if !res.Deleted || res.Revision != 2 {
		t.Fatalf("resource after delete = %+v", res)
	}
}

func TestStatsReflectState(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	registerNode(t, s, "node-b")
	now := time.Now().UTC()
	_, _ = s.ApplyPush(ctx, "node-a", []models.Change{upsert("a1", "dn-8", 0, now, `{}`)}, models.StrategyManual, nil)
	_, _ = s.ApplyPush(ctx, "node-b", []models.Change{upsert("b1", "dn-8", 0, now, `{}`)}, models.StrategyManual, nil)

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Nodes != 2 || st.Resources != 1 || st.PendingConflicts != 1 || st.LatestCursor == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// Rejected changes must still be recorded, or a node that resends a malformed
// change would be re-validated forever instead of being told it is a duplicate.
func TestInvalidChangeIsRejectedNotFatal(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool(t))
	registerNode(t, s, "node-a")
	bad := models.Change{ChangeID: "", ResourceType: "", ResourceID: ""}
	out, err := s.ApplyPush(ctx, "node-a", []models.Change{bad}, models.StrategyLastWriteWins, nil)
	if err != nil {
		t.Fatalf("invalid change should not fail the whole push: %v", err)
	}
	if out[0].Outcome != models.OutcomeRejected {
		t.Fatalf("outcome = %q, want rejected", out[0].Outcome)
	}
}
