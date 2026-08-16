// Package federation holds the merge logic that makes this service more than a
// write-through cache: deciding what happens when two places edited the same
// record.
//
// The decision is kept as a pure function over (change, current state,
// strategy) with no database or transport in sight. Convergence rules are the
// part that is easy to get subtly wrong and expensive to debug in production,
// so they are testable without a Postgres instance.
package federation

import (
	"github.com/iag/federation-gateway/internal/models"
)

// Verdict is what the engine decided to do with a submitted change.
type Verdict int

const (
	// Apply writes the node's change as the new authoritative revision.
	Apply Verdict = iota
	// Discard drops the node's change and keeps the central record. The push
	// still succeeds — the node is told its edit lost.
	Discard
	// Park records a conflict for a human and changes nothing.
	Park
)

// Decision is the engine's full answer for one change.
type Decision struct {
	Verdict Verdict
	// Conflicted reports whether the change diverged from the base the node
	// held. An Apply can be either a clean fast-forward (false) or the winner
	// of a resolved conflict (true) — the caller reports those differently.
	Conflicted bool
	Reason     string
}

// Outcome maps a decision onto the API-level outcome reported to the node.
func (d Decision) Outcome() models.ChangeOutcome {
	switch d.Verdict {
	case Apply:
		if d.Conflicted {
			return models.OutcomeConflictResolved
		}
		return models.OutcomeApplied
	case Park:
		return models.OutcomeConflictPending
	default:
		return models.OutcomeRejected
	}
}

// Decide arbitrates one change against the current central state.
//
// nodeID is the *authenticated* origin of the push, deliberately a parameter
// rather than a field on Change: a node must not be able to attribute its edits
// to a different node by crafting the request body.
//
// exists reports whether the resource is already known centrally; when false,
// current is ignored. A change is conflict-free exactly when the node's
// BaseRevision matches the revision the centre currently holds — that means the
// node edited the latest version and nothing raced it.
func Decide(nodeID string, change models.Change, current models.Resource, exists bool, strategy models.Strategy) Decision {
	if !strategy.Valid() {
		strategy = models.StrategyLastWriteWins
	}

	// First writer for this key. BaseRevision 0 is the honest claim ("I did not
	// know this record existed"); anything else means the node is referring to
	// a revision that has never existed centrally, which is a real divergence
	// rather than a fresh insert.
	if !exists {
		if change.BaseRevision == 0 {
			return Decision{Verdict: Apply, Reason: "new resource"}
		}
		return resolveConflict(change, current, strategy,
			"node references revision that does not exist centrally")
	}

	if change.BaseRevision == current.Revision {
		return Decision{Verdict: Apply, Reason: "fast-forward"}
	}

	// A node re-sending an edit against a base older than current, where the
	// current revision came from that same node, is a straggler rather than a
	// genuine multi-writer conflict — its own later state already won.
	if change.BaseRevision < current.Revision && current.OriginNodeID != "" &&
		current.OriginNodeID == nodeID {
		return Decision{
			Verdict:    Discard,
			Conflicted: true,
			Reason:     "superseded by a newer change from the same node",
		}
	}

	return resolveConflict(change, current, strategy, "base revision mismatch")
}

func resolveConflict(change models.Change, current models.Resource, strategy models.Strategy, why string) Decision {
	switch strategy {
	case models.StrategyServerWins:
		return Decision{Verdict: Discard, Conflicted: true, Reason: why + "; server_wins"}

	case models.StrategyNodeWins:
		return Decision{Verdict: Apply, Conflicted: true, Reason: why + "; node_wins"}

	case models.StrategyManual:
		return Decision{Verdict: Park, Conflicted: true, Reason: why + "; parked for manual resolution"}

	case models.StrategyLastWriteWins:
		// Compare the two edits' own wall-clock stamps. Ties go to the server:
		// with equal timestamps there is no evidence the node's edit is newer,
		// and preferring the incumbent makes the outcome deterministic rather
		// than dependent on arrival order.
		if change.UpdatedAt.After(current.OriginStampAt) {
			return Decision{Verdict: Apply, Conflicted: true, Reason: why + "; last_write_wins (node newer)"}
		}
		return Decision{Verdict: Discard, Conflicted: true, Reason: why + "; last_write_wins (server newer or tied)"}
	}

	return Decision{Verdict: Park, Conflicted: true, Reason: why}
}
