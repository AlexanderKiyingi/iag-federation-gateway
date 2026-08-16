package approvalchain

import "time"

// StepState is how one desk reads on a progress tracker.
type StepState string

const (
	StepPending  StepState = "pending"
	StepCurrent  StepState = "current"
	StepDone     StepState = "done"
	StepSkipped  StepState = "skipped"
	StepRejected StepState = "rejected"
)

// TrackerStep is one desk rendered on a request's progress bar. Skipped desks
// are included deliberately: a reader needs to see that GM was not consulted
// because the amount was below the band, rather than wonder where GM went.
type TrackerStep struct {
	Desk      DeskKey   `json:"desk"`
	Label     string    `json:"label"`
	State     StepState `json:"state"`
	Actor     string    `json:"actor,omitempty"`
	ActorRole string    `json:"actorRole,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Skipped   bool      `json:"skipped,omitempty"`
	MinAmount float64   `json:"minAmount,omitempty"`
	// ScopedTo is who this desk is narrowed to on this request, when it is
	// narrowed at all. It makes the difference between "any Project Manager"
	// and "this project's Project Manager" visible on the tracker, including
	// when the fallback applies and the desk is role-wide because the request
	// carries no owner.
	ScopedTo string `json:"scopedTo,omitempty"`
}

// Progress describes a request for a client: where it is, who it waits on, and
// the full desk-by-desk tracker.
type Progress struct {
	ChainKey    string        `json:"chainKey"`
	ChainLabel  string        `json:"chainLabel"`
	Status      Status        `json:"status"`
	StatusLabel string        `json:"statusLabel"`
	WaitingOn   string        `json:"waitingOn,omitempty"`
	Desk        DeskKey       `json:"desk,omitempty"`
	ActionLabel string        `json:"actionLabel,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Steps       []TrackerStep `json:"steps"`
}

// attemptStart is the index of the first step of the current attempt — the step
// after the most recent amendment, or 0 if the request has never been returned.
func attemptStart(history []Step) int {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Action == ActionAmend {
			return i + 1
		}
	}
	return 0
}

// WaitingOn returns the desk a request currently sits on.
func (e *Engine) WaitingOn(s State) (Desk, bool) {
	_, engaged, err := e.chainFor(s)
	if err != nil || s.Status != StatusInFlight {
		return Desk{}, false
	}
	return deskAt(s, engaged)
}

// Actionable reports whether this actor can advance the request right now.
// It is the predicate behind a desk queue: show me what is waiting on me.
func (e *Engine) Actionable(s State, a Actor) bool {
	desk, ok := e.WaitingOn(s)
	if !ok {
		return false
	}
	if !desk.HoldsFor(a, s.Scope) {
		return false
	}
	if !desk.AllowRequester && sameActor(a.ID, s.Requester) && !a.Admin {
		return false
	}
	return true
}

// Progress builds the tracker for a request.
func (e *Engine) Progress(s State) (Progress, error) {
	c, ok := e.reg.Get(s.ChainKey)
	if !ok {
		return Progress{}, ErrUnknownChain
	}
	opt := s.Options()
	engaged := c.Engaged(opt)

	// Where each engaged desk sits in the walk, so skipped desks can be
	// interleaved back into the full list in declaration order.
	pos := make(map[DeskKey]int, len(engaged))
	for i, d := range engaged {
		pos[d.Key] = i
	}

	// Last recorded action per desk, from the current attempt only.
	//
	// An amendment restarts the walk, so the desks that signed the previous
	// version are pending again. Carrying their old signatures forward would
	// render "pending" rows stamped with an approver and a date — which reads as
	// approved. History keeps the full record; the tracker shows this attempt.
	last := make(map[DeskKey]Step, len(s.History))
	for _, st := range s.History[attemptStart(s.History):] {
		if st.Action == ActionAdvance || st.Action == ActionReject || st.Action == ActionAmend {
			last[st.Desk] = st
		}
	}

	steps := make([]TrackerStep, 0, len(c.Desks))
	for _, d := range c.Desks {
		ts := TrackerStep{Desk: d.Key, Label: d.Label, MinAmount: d.MinAmount}
		if d.Scoped(s.Scope) {
			ts.ScopedTo = s.Scope[d.ScopeBy]
		}
		idx, engagedHere := pos[d.Key]
		switch {
		case !engagedHere:
			ts.State = StepSkipped
			ts.Skipped = true
		case s.Status == StatusRejected && idx == s.StageIndex:
			ts.State = StepRejected
		case s.Status == StatusApproved || idx < s.StageIndex:
			ts.State = StepDone
		case s.Status == StatusInFlight && idx == s.StageIndex:
			ts.State = StepCurrent
		default:
			ts.State = StepPending
		}
		if st, ok := last[d.Key]; ok {
			ts.Actor, ts.ActorRole, ts.At = st.Actor, st.ActorRole, st.At
			if st.Action != ActionAdvance {
				ts.Reason = st.Reason
			}
		}
		steps = append(steps, ts)
	}

	p := Progress{
		ChainKey:   c.Key,
		ChainLabel: c.Label,
		Status:     s.Status,
		Steps:      steps,
		Reason:     s.LastReason(),
	}

	switch s.Status {
	case StatusApproved:
		p.StatusLabel = c.Terminal()
	case StatusRejected:
		p.StatusLabel = "Rejected"
	case StatusCancelled:
		p.StatusLabel = "Cancelled"
	case StatusReturned:
		p.StatusLabel = "Returned for Amendment"
		p.WaitingOn = "Requestor"
	case StatusDraft:
		p.StatusLabel = "Draft"
		p.WaitingOn = "Requestor"
	case StatusInFlight:
		if d, ok := deskAt(s, engaged); ok {
			p.Desk = d.Key
			p.WaitingOn = d.Label
			p.ActionLabel = d.Action()
			if s.StageIndex > 0 {
				p.StatusLabel = engaged[s.StageIndex-1].PassedStatus()
			} else {
				p.StatusLabel = "Submitted"
			}
		}
	}
	return p, nil
}
