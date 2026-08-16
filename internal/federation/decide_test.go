package federation

import (
	"testing"
	"time"

	"github.com/iag/federation-gateway/internal/models"
)

var (
	t0 = time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func change(base int64, stamp time.Time) models.Change {
	return models.Change{
		ChangeID:     "c1",
		ResourceType: "delivery_note",
		ResourceID:   "dn-1",
		Op:           models.OpUpsert,
		BaseRevision: base,
		UpdatedAt:    stamp,
	}
}

func resource(rev int64, origin string, stamp time.Time) models.Resource {
	return models.Resource{
		ResourceType:  "delivery_note",
		ResourceID:    "dn-1",
		Revision:      rev,
		OriginNodeID:  origin,
		OriginStampAt: stamp,
	}
}

func TestDecide_newResourceApplies(t *testing.T) {
	d := Decide("node-a", change(0, t0), models.Resource{}, false, models.StrategyLastWriteWins)
	if d.Verdict != Apply || d.Conflicted {
		t.Fatalf("new resource: got %+v, want clean Apply", d)
	}
	if d.Outcome() != models.OutcomeApplied {
		t.Fatalf("outcome = %q, want applied", d.Outcome())
	}
}

// A node claiming a base revision for a resource the centre has never seen is
// not a fresh insert — it means state diverged (or was purged centrally), so it
// must go through conflict handling rather than silently creating a record.
func TestDecide_unknownResourceWithBaseRevisionConflicts(t *testing.T) {
	d := Decide("node-a", change(7, t0), models.Resource{}, false, models.StrategyManual)
	if d.Verdict != Park || !d.Conflicted {
		t.Fatalf("phantom base revision: got %+v, want parked conflict", d)
	}
}

func TestDecide_fastForwardApplies(t *testing.T) {
	d := Decide("node-a", change(3, t1), resource(3, "node-b", t0), true, models.StrategyManual)
	if d.Verdict != Apply || d.Conflicted {
		t.Fatalf("matching base revision: got %+v, want clean Apply", d)
	}
}

func TestDecide_strategies(t *testing.T) {
	cur := resource(5, "node-b", t1)
	stale := change(3, t0) // node edited an older revision, earlier stamp

	cases := []struct {
		strategy   models.Strategy
		wantVerd   Verdict
		wantOutcom models.ChangeOutcome
	}{
		{models.StrategyServerWins, Discard, models.OutcomeRejected},
		{models.StrategyNodeWins, Apply, models.OutcomeConflictResolved},
		{models.StrategyManual, Park, models.OutcomeConflictPending},
		{models.StrategyLastWriteWins, Discard, models.OutcomeRejected}, // server stamp newer
	}
	for _, tc := range cases {
		d := Decide("node-a", stale, cur, true, tc.strategy)
		if d.Verdict != tc.wantVerd {
			t.Errorf("%s: verdict = %v, want %v (%s)", tc.strategy, d.Verdict, tc.wantVerd, d.Reason)
		}
		if !d.Conflicted {
			t.Errorf("%s: expected Conflicted=true", tc.strategy)
		}
		if got := d.Outcome(); got != tc.wantOutcom {
			t.Errorf("%s: outcome = %q, want %q", tc.strategy, got, tc.wantOutcom)
		}
	}
}

func TestDecide_lastWriteWinsPrefersNewerNodeEdit(t *testing.T) {
	// Node edited an older base but stamped it later than the server's edit.
	d := Decide("node-a", change(3, t1), resource(5, "node-b", t0), true, models.StrategyLastWriteWins)
	if d.Verdict != Apply || !d.Conflicted {
		t.Fatalf("newer node edit: got %+v, want conflicted Apply", d)
	}
}

// Equal timestamps must be deterministic. Preferring the incumbent means the
// result does not depend on which node's request happened to arrive first.
func TestDecide_lastWriteWinsTieKeepsServer(t *testing.T) {
	d := Decide("node-a", change(3, t0), resource(5, "node-b", t0), true, models.StrategyLastWriteWins)
	if d.Verdict != Discard {
		t.Fatalf("tied stamps: got %+v, want Discard (server keeps)", d)
	}
}

// A node re-pushing against its own already-superseded revision is a straggler,
// not a genuine two-writer conflict; it must not raise a conflict for a human.
func TestDecide_sameNodeStragglerIsDiscardedNotParked(t *testing.T) {
	d := Decide("node-a", change(3, t0), resource(5, "node-a", t1), true, models.StrategyManual)
	if d.Verdict != Discard {
		t.Fatalf("same-node straggler: got %+v, want Discard", d)
	}
	if d.Reason == "" {
		t.Error("expected a reason explaining the discard")
	}
}

// A different node editing the same base is a real conflict even though the
// base revisions are equally stale — this is the case the straggler rule must
// not swallow.
func TestDecide_otherNodeStaleIsRealConflict(t *testing.T) {
	d := Decide("node-c", change(3, t0), resource(5, "node-a", t1), true, models.StrategyManual)
	if d.Verdict != Park {
		t.Fatalf("cross-node conflict: got %+v, want Park", d)
	}
}

func TestDecide_unknownStrategyFallsBackToLastWriteWins(t *testing.T) {
	d := Decide("node-a", change(3, t1), resource(5, "node-b", t0), true, models.Strategy("nonsense"))
	if d.Verdict != Apply {
		t.Fatalf("unknown strategy: got %+v, want last-write-wins behaviour", d)
	}
}

func TestChangeNormalize(t *testing.T) {
	c := models.Change{ResourceType: "  Delivery_Note ", ResourceID: " dn-1 ", ChangeID: " x "}
	c.Normalize()
	if c.ResourceType != "delivery_note" || c.ResourceID != "dn-1" || c.ChangeID != "x" {
		t.Fatalf("normalize = %+v", c)
	}
}
