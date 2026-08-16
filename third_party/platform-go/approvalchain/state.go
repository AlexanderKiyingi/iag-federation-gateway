package approvalchain

import (
	"strings"
	"time"
)

// Status is where a request stands overall.
type Status string

const (
	// StatusDraft is a request the requester has not submitted yet.
	StatusDraft Status = "draft"
	// StatusInFlight is a request sitting on a desk.
	StatusInFlight Status = "in_flight"
	// StatusReturned is a request sent back to the requester for amendment.
	StatusReturned Status = "returned_for_amendment"
	// StatusApproved is a request that cleared every engaged desk.
	StatusApproved Status = "approved"
	// StatusRejected is a request a desk refused.
	StatusRejected Status = "rejected"
	// StatusCancelled is a request the requester withdrew.
	StatusCancelled Status = "cancelled"
)

// Open reports whether the request can still move.
func (s Status) Open() bool {
	return s == StatusDraft || s == StatusInFlight || s == StatusReturned
}

// Terminal reports whether the request has finished, one way or another.
func (s Status) Terminal() bool {
	return s == StatusApproved || s == StatusRejected || s == StatusCancelled
}

// Action is a transition recorded in history.
type Action string

const (
	ActionSubmit  Action = "submit"
	ActionAdvance Action = "advance"
	ActionReject  Action = "reject"
	ActionAmend   Action = "amend"
	ActionCancel  Action = "cancel"
)

// Step is one entry in a request's approval history. Every transition appends
// one, so history is the audit trail: who acted, at which desk, in what role,
// and — for a rejection or an amendment — why.
type Step struct {
	Desk      DeskKey   `json:"desk"`
	DeskLabel string    `json:"deskLabel"`
	Action    Action    `json:"action"`
	Actor     string    `json:"actor"`
	ActorRole string    `json:"actorRole"`
	Reason    string    `json:"reason,omitempty"`
	Override  bool      `json:"override,omitempty"`
	At        time.Time `json:"at"`
}

// State is a request's position in its chain. It is a value: transitions return
// a new State rather than mutating, so a failed transition cannot leave a
// half-applied one behind.
type State struct {
	ChainKey   string    `json:"chainKey"`
	Status     Status    `json:"status"`
	Desk       DeskKey   `json:"desk,omitempty"`
	StageIndex int       `json:"stageIndex"`
	Amount     float64   `json:"amount"`
	Skip       []DeskKey `json:"skip,omitempty"`
	Requester  string    `json:"requester"`
	History    []Step    `json:"history"`
	// Scope carries who this request belongs to, keyed by the names desks use in
	// ScopeBy — for example {"project_owner": "alice@iag.local", "department":
	// "Ops"}. Captured when the chain opens, because ownership at submission is
	// what the approval refers to.
	Scope map[string]string `json:"scope,omitempty"`
}

// Actor is whoever is trying to move the request.
type Actor struct {
	// ID identifies the person. Compared against State.Requester for four-eyes.
	ID string
	// Roles are matched against each desk's role patterns. A plural because
	// platform tokens carry a roles array — one person can hold two desks.
	Roles []string
	// Admin lets the actor act on any desk. Every admin action is recorded with
	// Override set, so a bypass is always visible in history.
	Admin bool
	// HasPerm answers whether the actor carries a permission code, enforcing
	// each desk's RequiredPerm. Nil opts out of permission gating and leaves
	// role matching as the only check.
	HasPerm HasPerm
	// Attrs are the actor's own scope values, keyed like Desk.ScopeBy — for
	// example {"department": "Ops"}. They let a desk be scoped to a group the
	// actor belongs to rather than only to the actor themselves.
	Attrs map[string]string
}

// ActorWithRole is the single-role convenience constructor.
func ActorWithRole(id, role string) Actor {
	return Actor{ID: id, Roles: []string{role}}
}

// New starts a request in draft against a chain.
func New(chainKey, requester string, opt Options) State {
	return State{
		ChainKey:   chainKey,
		Status:     StatusDraft,
		StageIndex: -1,
		Amount:     opt.Amount,
		Skip:       append([]DeskKey(nil), opt.Skip...),
		Requester:  strings.TrimSpace(requester),
		History:    nil,
		Scope:      cloneScope(opt.Scope),
	}
}

// cloneScope copies a scope map, dropping blank values so a missing attribute
// and an empty one are the same thing to every caller.
func cloneScope(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Options reconstructs the request shape this state was opened with, so callers
// need not carry amount and skips separately.
func (s State) Options() Options {
	return Options{Amount: s.Amount, Skip: s.Skip, Scope: s.Scope}
}

// LastReason returns the most recent rejection or amendment reason, which is
// what a UI shows on a returned or rejected request.
func (s State) LastReason() string {
	for i := len(s.History) - 1; i >= 0; i-- {
		st := s.History[i]
		if st.Action == ActionReject || st.Action == ActionAmend {
			return st.Reason
		}
	}
	return ""
}

// clone copies the state so a transition never aliases the caller's history.
func (s State) clone() State {
	out := s
	out.Skip = append([]DeskKey(nil), s.Skip...)
	out.History = append([]Step(nil), s.History...)
	out.Scope = cloneScope(s.Scope)
	return out
}

func (s *State) record(d Desk, action Action, a Actor, reason string, override bool, now time.Time) {
	s.History = append(s.History, Step{
		Desk:      d.Key,
		DeskLabel: d.Label,
		Action:    action,
		Actor:     a.ID,
		ActorRole: d.byRole(a),
		Reason:    strings.TrimSpace(reason),
		Override:  override,
		At:        now.UTC(),
	})
}
