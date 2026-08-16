package approvalchain

import (
	"strings"
	"time"
)

// Clock lets tests pin transition timestamps. Zero value uses time.Now.
type Clock func() time.Time

func (c Clock) now() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c().UTC()
}

// Engine applies transitions against a registry. Construct one per service.
type Engine struct {
	reg   *Registry
	clock Clock
}

// NewEngine returns an engine over the given registry.
func NewEngine(reg *Registry) *Engine { return &Engine{reg: reg} }

// WithClock returns a copy of the engine using the given clock. Test-only in
// practice, but harmless in production.
func (e *Engine) WithClock(c Clock) *Engine {
	return &Engine{reg: e.reg, clock: c}
}

// Registry exposes the chains this engine routes.
func (e *Engine) Registry() *Registry { return e.reg }

func (e *Engine) chainFor(s State) (*Chain, []Desk, error) {
	c, ok := e.reg.Get(s.ChainKey)
	if !ok {
		return nil, nil, ErrUnknownChain
	}
	engaged := c.Engaged(s.Options())
	if len(engaged) == 0 && !c.AllowAutoApprove {
		return nil, nil, ErrNoEngagedDesks
	}
	return c, engaged, nil
}

// Submit moves a draft (or a returned request) onto its first engaged desk.
//
// Only the requester may submit. A chain whose bands leave no engaged desk and
// which allows auto-approval completes here — that is how "below every band, no
// approval needed" is expressed.
func (e *Engine) Submit(s State, a Actor) (State, error) {
	c, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	if s.Status != StatusDraft && s.Status != StatusReturned {
		return s, ErrNotWaiting
	}
	if !a.Admin && !sameActor(a.ID, s.Requester) {
		return s, ErrNotRequester
	}

	out := s.clone()
	now := e.clock.now()

	if len(engaged) == 0 {
		out.Status = StatusApproved
		out.Desk = ""
		out.StageIndex = 0
		out.record(Desk{Key: "", Label: c.Terminal()}, ActionSubmit, a, "", a.Admin && !sameActor(a.ID, s.Requester), now)
		return out, nil
	}

	first := engaged[0]
	out.Status = StatusInFlight
	out.StageIndex = 0
	out.Desk = first.Key
	out.record(first, ActionSubmit, a, "", false, now)
	return out, nil
}

// Rebase re-evaluates a request against its current amount.
//
// It exists because the amount is not frozen: most services let a requisition be
// edited while it is in flight. If the chain kept the amount it opened with, a
// request could be raised to 50M after opening at 1M and still walk the
// small-request desks — the bands would be bypassed entirely. Callers therefore
// rebase against the live amount before every transition.
//
// The cursor is re-anchored by desk key rather than by index, so a request
// sitting on Accounts stays on Accounts even when a raised amount inserts GM and
// CEO after it. If the new bands drop the desk the request is sitting on, there
// is no honest place to put it: the amount changed under an approver, so
// ErrAmountChanged asks the caller to return it for amendment instead.
func (e *Engine) Rebase(s State, amount float64) (State, error) {
	if amount == s.Amount {
		return s, nil
	}
	c, _, err := e.chainFor(s)
	if err != nil {
		return s, err
	}

	out := s.clone()
	out.Amount = amount
	if s.Status != StatusInFlight {
		return out, nil
	}

	engaged := c.Engaged(out.Options())
	for i, d := range engaged {
		if d.Key == s.Desk {
			out.StageIndex = i
			return out, nil
		}
	}
	return s, ErrAmountChanged
}

// Advance passes the request from its current desk to the next engaged one, or
// to approved when the current desk is the last.
//
// The actor must hold the current desk by role, or be an admin — in which case
// the step is recorded with Override set so the bypass stays visible. A
// requester cannot advance their own request unless the desk allows it.
func (e *Engine) Advance(s State, a Actor, note string) (State, error) {
	c, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	desk, err := currentDesk(s, engaged)
	if err != nil {
		return s, err
	}
	if !desk.HoldsFor(a, s.Scope) {
		return s, ErrForbidden
	}
	if err := desk.checkPerm(a); err != nil {
		return s, err
	}
	if !desk.AllowRequester && sameActor(a.ID, s.Requester) && !a.Admin {
		return s, ErrSelfApproval
	}
	if c.NoSuccessiveApprover && !a.Admin && sameActor(a.ID, previousApprover(s)) {
		return s, ErrSelfSuccession
	}
	if c.NoRepeatApprover && !a.Admin && hasAlreadyAdvanced(s, a.ID) {
		return s, ErrRepeatApprover
	}

	out := s.clone()
	now := e.clock.now()
	out.record(desk, ActionAdvance, a, note, a.Admin && !deskHeldByRole(desk, a), now)

	if s.StageIndex+1 >= len(engaged) {
		out.Status = StatusApproved
		out.Desk = ""
		out.StageIndex = len(engaged)
		return out, nil
	}
	next := engaged[s.StageIndex+1]
	out.StageIndex = s.StageIndex + 1
	out.Desk = next.Key
	return out, nil
}

// Reject refuses the request outright. The reason is mandatory: a desk that
// cannot say why it refused gives the requester nothing to act on, which is the
// failure mode this whole model exists to prevent.
func (e *Engine) Reject(s State, a Actor, reason string) (State, error) {
	_, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	desk, err := currentDesk(s, engaged)
	if err != nil {
		return s, err
	}
	if !desk.HoldsFor(a, s.Scope) {
		return s, ErrForbidden
	}
	if strings.TrimSpace(reason) == "" {
		return s, ErrReasonRequired
	}

	out := s.clone()
	out.Status = StatusRejected
	out.Desk = desk.Key
	out.record(desk, ActionReject, a, reason, a.Admin && !deskHeldByRole(desk, a), e.clock.now())
	return out, nil
}

// Amend returns the request to its requester for correction, keeping it alive.
// Like a rejection it requires a reason, and it resets the walk: a resubmitted
// request starts again at the first engaged desk, because an amended request is
// not the one the earlier desks approved.
func (e *Engine) Amend(s State, a Actor, reason string) (State, error) {
	_, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	desk, err := currentDesk(s, engaged)
	if err != nil {
		return s, err
	}
	if !desk.HoldsFor(a, s.Scope) {
		return s, ErrForbidden
	}
	if strings.TrimSpace(reason) == "" {
		return s, ErrReasonRequired
	}

	out := s.clone()
	out.Status = StatusReturned
	out.Desk = desk.Key
	out.StageIndex = -1
	out.record(desk, ActionAmend, a, reason, a.Admin && !deskHeldByRole(desk, a), e.clock.now())
	return out, nil
}

// Cancel withdraws the request. Only the requester or an admin may cancel, and
// only while it is still open.
func (e *Engine) Cancel(s State, a Actor, reason string) (State, error) {
	c, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	if !s.Status.Open() {
		return s, ErrNotWaiting
	}
	if !a.Admin && !sameActor(a.ID, s.Requester) {
		return s, ErrNotRequester
	}

	// Withdrawing your own request needs no justification — it is yours. Killing
	// someone else's does. That path is an administrative override, and without a
	// reason the record shows an administrator ended a request and says nothing
	// about why, which is exactly the question an audit asks first.
	override := a.Admin && !sameActor(a.ID, s.Requester)
	if override && strings.TrimSpace(reason) == "" {
		return s, ErrReasonRequired
	}

	desk := Desk{Key: s.Desk, Label: c.Label}
	if d, ok := deskAt(s, engaged); ok {
		desk = d
	}

	out := s.clone()
	out.Status = StatusCancelled
	out.record(desk, ActionCancel, a, reason, override, e.clock.now())
	return out, nil
}

// Reopen puts a rejected request back on the desk that refused it. Admin-only,
// and recorded as an override — it exists because a desk that rejects in error
// should not force the requester to re-key the whole request.
func (e *Engine) Reopen(s State, a Actor, reason string) (State, error) {
	_, engaged, err := e.chainFor(s)
	if err != nil {
		return s, err
	}
	if s.Status != StatusRejected {
		return s, ErrNotWaiting
	}
	if !a.Admin {
		return s, ErrForbidden
	}
	if strings.TrimSpace(reason) == "" {
		return s, ErrReasonRequired
	}
	if s.StageIndex < 0 || s.StageIndex >= len(engaged) {
		return s, ErrWrongDesk
	}

	desk := engaged[s.StageIndex]
	out := s.clone()
	out.Status = StatusInFlight
	out.Desk = desk.Key
	out.record(desk, ActionSubmit, a, reason, true, e.clock.now())
	return out, nil
}

func currentDesk(s State, engaged []Desk) (Desk, error) {
	if s.Status != StatusInFlight {
		return Desk{}, ErrNotWaiting
	}
	d, ok := deskAt(s, engaged)
	if !ok {
		return Desk{}, ErrWrongDesk
	}
	return d, nil
}

func deskAt(s State, engaged []Desk) (Desk, bool) {
	if s.StageIndex < 0 || s.StageIndex >= len(engaged) {
		return Desk{}, false
	}
	d := engaged[s.StageIndex]
	// Guard against a chain edited while requests were in flight: the cursor
	// must still point at the desk the state recorded.
	if s.Desk != "" && d.Key != s.Desk {
		return Desk{}, false
	}
	return d, true
}

func sameActor(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}

// previousApprover is who cleared the desk before the current one, in this
// attempt. System actors return "" so an automated advance never blocks the
// next human — a rule about separating people should not fire on a machine.
func previousApprover(s State) string {
	for i := len(s.History) - 1; i >= attemptStart(s.History); i-- {
		if s.History[i].Action != ActionAdvance {
			continue
		}
		if isSystemActor(s.History[i].Actor) {
			return ""
		}
		return s.History[i].Actor
	}
	return ""
}

// hasAlreadyAdvanced reports whether this actor has cleared any desk on the
// current attempt. An amendment restarts the walk, so signatures on a superseded
// version do not bar someone from approving the corrected one.
func hasAlreadyAdvanced(s State, id string) bool {
	if isSystemActor(id) {
		return false
	}
	for _, st := range s.History[attemptStart(s.History):] {
		if st.Action == ActionAdvance && sameActor(id, st.Actor) {
			return true
		}
	}
	return false
}

// isSystemActor treats an empty or "system" actor as non-human: automated and
// unattributed advances are exempt from segregation-of-duties rules.
func isSystemActor(by string) bool {
	b := strings.TrimSpace(by)
	return b == "" || strings.EqualFold(b, "system")
}
