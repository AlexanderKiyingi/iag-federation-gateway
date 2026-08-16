package approvalchain

import (
	"errors"
	"testing"
	"time"
)

// requisition mirrors the chain financialtooliag runs:
// Requestor → PM → Accounts Assistant → GM → CEO → Finance → Paid,
// with GM and CEO gated on amount bands.
func requisitionChain() Chain {
	return Chain{
		Key:           "requisition",
		Label:         "Requestor → PM → Accounts → GM → CEO → Finance → Paid",
		TerminalLabel: "Paid",
		Desks: []Desk{
			{Key: "pm", Label: "Project Manager", RolePatterns: []string{`project\s*manager`, `\bpm\b`}},
			{Key: "accounts", Label: "Accounts Assistant", RolePatterns: []string{`accounts?\s*assistant`, `\baccountant\b`}},
			{Key: "gm", Label: "General Manager", RolePatterns: []string{`general\s*manager`, `\bgm\b`}, MinAmount: 5_000_000},
			{Key: "ceo", Label: "CEO", RolePatterns: []string{`\bceo\b`, `chief\s*executive`}, MinAmount: 20_000_000},
			{Key: "finance", Label: "Finance", RolePatterns: []string{`\bfinance\b`, `cashier`, `treasurer`},
				ActionLabel: "Make payment", StatusLabel: "Paid"},
		},
	}
}

// stepper unwraps a transition, failing the test on error. It takes exactly the
// (State, error) pair a transition returns so calls can be written as
// step(e.Advance(s, pm, "")).
type stepper func(State, error) State

func newStepper(t *testing.T) stepper {
	return func(s State, err error) State {
		t.Helper()
		if err != nil {
			t.Fatalf("transition failed: %v", err)
		}
		return s
	}
}

func testEngine(t *testing.T) (*Engine, stepper) {
	t.Helper()
	reg, err := NewRegistry(requisitionChain())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	at := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	return NewEngine(reg).WithClock(func() time.Time { return at }), newStepper(t)
}

var (
	requester = ActorWithRole("u-req", "Clerk")
	pm        = ActorWithRole("u-pm", "Project Manager")
	accounts  = ActorWithRole("u-acc", "Accounts Assistant")
	gm        = ActorWithRole("u-gm", "GM")
	ceo       = ActorWithRole("u-ceo", "CEO")
	finance   = ActorWithRole("u-fin", "Finance Officer")
	admin     = Actor{ID: "u-adm", Roles: []string{"Administrator"}, Admin: true}
)

func TestBandsDecideWhichDesksEngage(t *testing.T) {
	e, _ := testEngine(t)

	cases := []struct {
		name   string
		amount float64
		want   []DeskKey
	}{
		{"below every band stops at accounts", 1_000_000, []DeskKey{"pm", "accounts", "finance"}},
		{"mid band adds GM", 9_000_000, []DeskKey{"pm", "accounts", "gm", "finance"}},
		{"top band adds CEO", 25_000_000, []DeskKey{"pm", "accounts", "gm", "ceo", "finance"}},
		{"exactly on the band engages it", 5_000_000, []DeskKey{"pm", "accounts", "gm", "finance"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := e.Registry().Get("requisition")
			engaged := c.Engaged(Options{Amount: tc.amount})
			if len(engaged) != len(tc.want) {
				t.Fatalf("engaged %d desks, want %d (%v)", len(engaged), len(tc.want), deskKeys(engaged))
			}
			for i, d := range engaged {
				if d.Key != tc.want[i] {
					t.Fatalf("desk %d = %q, want %q", i, d.Key, tc.want[i])
				}
			}
		})
	}
}

func TestSkipRemovesADeskWithoutASecondChain(t *testing.T) {
	e, _ := testEngine(t)
	c, _ := e.Registry().Get("requisition")
	engaged := c.Engaged(Options{Amount: 25_000_000, Skip: []DeskKey{"pm"}})
	if got := deskKeys(engaged); len(got) != 4 || got[0] != "accounts" {
		t.Fatalf("engaged = %v, want the chain without pm", got)
	}
}

func TestFullWalkToApproved(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 25_000_000})

	s = step(e.Submit(s, requester))
	if s.Status != StatusInFlight || s.Desk != "pm" {
		t.Fatalf("after submit: %s on %q, want in_flight on pm", s.Status, s.Desk)
	}

	for _, a := range []Actor{pm, accounts, gm, ceo} {
		s = step(e.Advance(s, a, ""))
		if s.Status != StatusInFlight {
			t.Fatalf("chain closed early at %v", a.Roles)
		}
	}
	if s.Desk != "finance" {
		t.Fatalf("desk = %q, want finance", s.Desk)
	}

	s = step(e.Advance(s, finance, "paid by EFT"))
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}
	// submit + five desk advances
	if len(s.History) != 6 {
		t.Fatalf("history has %d steps, want 6", len(s.History))
	}
}

func TestSmallRequestSkipsGMAndCEOEndToEnd(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 250_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Advance(s, accounts, ""))

	if s.Desk != "finance" {
		t.Fatalf("desk = %q, want finance — GM and CEO are above the band", s.Desk)
	}
	if _, err := e.Advance(s, gm, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GM advancing an unbanded request: %v, want ErrForbidden", err)
	}
	s = step(e.Advance(s, finance, ""))
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}
}

func TestWrongRoleCannotAdvance(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 25_000_000})
	s = step(e.Submit(s, requester))

	if _, err := e.Advance(s, ceo, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CEO jumping the PM desk: %v, want ErrForbidden", err)
	}
}

func TestRequesterCannotApproveOwnRequest(t *testing.T) {
	e, step := testEngine(t)
	// The requester also happens to hold the PM desk.
	self := ActorWithRole("u-pm", "Project Manager")
	s := New("requisition", self.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, self))

	if _, err := e.Advance(s, self, ""); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval: %v, want ErrSelfApproval", err)
	}
}

func TestRejectRequiresAReason(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))

	if _, err := e.Reject(s, pm, "   "); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("blank reject reason: %v, want ErrReasonRequired", err)
	}

	out := step(e.Reject(s, pm, "no budget line"))
	if out.Status != StatusRejected {
		t.Fatalf("status = %s, want rejected", out.Status)
	}
	if out.LastReason() != "no budget line" {
		t.Fatalf("reason = %q, want the rejection reason", out.LastReason())
	}
}

func TestAmendRequiresAReasonAndRestartsTheWalk(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 25_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Advance(s, accounts, ""))

	if _, err := e.Amend(s, gm, ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("blank amend reason: %v, want ErrReasonRequired", err)
	}

	s = step(e.Amend(s, gm, "attach the supplier quote"))
	if s.Status != StatusReturned {
		t.Fatalf("status = %s, want returned_for_amendment", s.Status)
	}

	// An amended request is not the one PM and Accounts approved, so it starts
	// over rather than resuming at GM.
	s = step(e.Submit(s, requester))
	if s.Desk != "pm" {
		t.Fatalf("resubmitted onto %q, want pm — the walk must restart", s.Desk)
	}
}

func TestOnlyRequesterMaySubmitOrCancel(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})

	if _, err := e.Submit(s, pm); !errors.Is(err, ErrNotRequester) {
		t.Fatalf("submit by a stranger: %v, want ErrNotRequester", err)
	}
	s = step(e.Submit(s, requester))
	if _, err := e.Cancel(s, accounts, ""); !errors.Is(err, ErrNotRequester) {
		t.Fatalf("cancel by a stranger: %v, want ErrNotRequester", err)
	}
	s = step(e.Cancel(s, requester, "duplicate"))
	if s.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", s.Status)
	}
}

func TestAdminOverrideIsAllowedAndRecorded(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))

	s = step(e.Advance(s, admin, "PM on leave"))
	if s.Desk != "accounts" {
		t.Fatalf("desk = %q, want accounts", s.Desk)
	}
	last := s.History[len(s.History)-1]
	if !last.Override {
		t.Fatal("an admin bypassing a desk must be recorded as an override")
	}
	if last.Actor != admin.ID {
		t.Fatalf("step actor = %q, want the admin", last.Actor)
	}
}

func TestTerminalRequestsRefuseFurtherTransitions(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Reject(s, pm, "not needed"))

	if _, err := e.Advance(s, pm, ""); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("advancing a rejected request: %v, want ErrNotWaiting", err)
	}
	if _, err := e.Submit(s, requester); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("resubmitting a rejected request: %v, want ErrNotWaiting", err)
	}
}

func TestReopenIsAdminOnlyAndReturnsToTheRefusingDesk(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Reject(s, accounts, "wrong cost centre"))

	if _, err := e.Reopen(s, accounts, "fixed"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin reopen: %v, want ErrForbidden", err)
	}
	s = step(e.Reopen(s, admin, "cost centre corrected by finance"))
	if s.Status != StatusInFlight || s.Desk != "accounts" {
		t.Fatalf("reopened to %s/%q, want in_flight on accounts", s.Status, s.Desk)
	}
}

func TestActionableDrivesTheDeskQueue(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 25_000_000})
	s = step(e.Submit(s, requester))

	if !e.Actionable(s, pm) {
		t.Fatal("PM should see a request sitting on the PM desk")
	}
	for _, a := range []Actor{gm, ceo, finance, requester} {
		if e.Actionable(s, a) {
			t.Fatalf("%v should not see a request on the PM desk", a.Roles)
		}
	}

	s = step(e.Advance(s, pm, ""))
	if !e.Actionable(s, accounts) || e.Actionable(s, pm) {
		t.Fatal("the queue should have moved from PM to Accounts")
	}
}

func TestProgressMarksBandedDesksSkippedNotMissing(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))

	p, err := e.Progress(s)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if len(p.Steps) != 5 {
		t.Fatalf("tracker has %d steps, want all 5 desks including skipped ones", len(p.Steps))
	}
	want := map[DeskKey]StepState{
		"pm": StepDone, "accounts": StepCurrent,
		"gm": StepSkipped, "ceo": StepSkipped, "finance": StepPending,
	}
	for _, st := range p.Steps {
		if want[st.Desk] != st.State {
			t.Errorf("desk %q is %q, want %q", st.Desk, st.State, want[st.Desk])
		}
	}
	if p.WaitingOn != "Accounts Assistant" {
		t.Errorf("waitingOn = %q, want Accounts Assistant", p.WaitingOn)
	}
	if p.ActionLabel != "Approve" {
		t.Errorf("actionLabel = %q, want Approve", p.ActionLabel)
	}
}

func TestProgressUsesTheDeskActionAndTerminalLabels(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Advance(s, accounts, ""))

	p, _ := e.Progress(s)
	if p.ActionLabel != "Make payment" {
		t.Errorf("actionLabel = %q, want the Finance desk's own label", p.ActionLabel)
	}

	s = step(e.Advance(s, finance, ""))
	p, _ = e.Progress(s)
	if p.StatusLabel != "Paid" {
		t.Errorf("statusLabel = %q, want the chain terminal label Paid", p.StatusLabel)
	}
}

func TestTransitionsDoNotMutateTheInputState(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	before := len(s.History)

	if _, err := e.Advance(s, pm, ""); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(s.History) != before {
		t.Fatal("a transition must not append to the caller's history")
	}
	if s.Desk != "pm" {
		t.Fatal("a transition must not move the caller's cursor")
	}
}

func TestRegistryRejectsUnroutableChains(t *testing.T) {
	cases := []struct {
		name  string
		chain Chain
	}{
		{"no desks", Chain{Key: "empty"}},
		{"desk with no roles", Chain{Key: "c", Desks: []Desk{{Key: "a", Label: "A"}}}},
		{"duplicate desk", Chain{Key: "c", Desks: []Desk{
			{Key: "a", RolePatterns: []string{"x"}}, {Key: "a", RolePatterns: []string{"y"}},
		}}},
		{"every desk banded", Chain{Key: "c", Desks: []Desk{
			{Key: "a", RolePatterns: []string{"x"}, MinAmount: 10},
		}}},
		{"bad role pattern", Chain{Key: "c", Desks: []Desk{{Key: "a", RolePatterns: []string{"("}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry(tc.chain); err == nil {
				t.Fatal("expected the registry to reject this chain")
			}
		})
	}
}

func TestAutoApproveChainCompletesOnSubmit(t *testing.T) {
	reg, err := NewRegistry(Chain{
		Key: "petty", Label: "Petty cash", TerminalLabel: "Paid", AllowAutoApprove: true,
		Desks: []Desk{{Key: "gm", Label: "GM", RolePatterns: []string{`\bgm\b`}, MinAmount: 1_000_000}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	s := New("petty", requester.ID, Options{Amount: 20_000})
	s = step(e.Submit(s, requester))
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved — nothing engages below the band", s.Status)
	}
}

func TestUnknownChainIsAnError(t *testing.T) {
	e, _ := testEngine(t)
	if _, err := e.Submit(New("gone", "u", Options{}), requester); !errors.Is(err, ErrUnknownChain) {
		t.Fatalf("unknown chain: %v, want ErrUnknownChain", err)
	}
}

func TestDesksForRoleBuildsTheQueueFilter(t *testing.T) {
	e, _ := testEngine(t)
	got := e.Registry().DesksForRole("General Manager")
	desks := got["requisition"]
	if len(desks) != 1 || desks[0] != "gm" {
		t.Fatalf("DesksForRole = %v, want just gm", desks)
	}
	if len(e.Registry().DesksForRole("Warehouse Clerk")) != 0 {
		t.Fatal("a role holding no desk should match no chains")
	}
}

func TestTrackerDoesNotCarryOldSignaturesPastAnAmendment(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Amend(s, accounts, "attach the quote"))
	s = step(e.Submit(s, requester))

	p, err := e.Progress(s)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	for _, st := range p.Steps {
		if st.Desk == "pm" {
			if st.State != StepCurrent {
				t.Fatalf("pm is %q after resubmission, want current", st.State)
			}
			// The old PM approval must not be shown against a desk that has to
			// approve again — it would read as already signed.
			if st.Actor != "" || !st.At.IsZero() {
				t.Errorf("pm still shows the pre-amendment signature by %q at %v", st.Actor, st.At)
			}
		}
	}
	// The record itself is not lost — history keeps every attempt.
	if len(s.History) != 4 {
		t.Errorf("history has %d steps, want 4 — the tracker hides old attempts, it does not erase them", len(s.History))
	}
}

// scopedChain narrows the PM desk to the project's assigned manager — the
// semantic the source ERP had and a role-global desk does not.
func scopedChain() Chain {
	return Chain{
		Key: "scoped", Label: "Scoped", TerminalLabel: "Approved",
		Desks: []Desk{
			{Key: "pm", Label: "Project Manager", RolePatterns: []string{`project\s*manager`, `\bpm\b`},
				ScopeBy: "project_owner"},
			{Key: "head", Label: "Department Head", RolePatterns: []string{`department\s*head`},
				ScopeBy: "department"},
			{Key: "finance", Label: "Finance", RolePatterns: []string{`\bfinance\b`}},
		},
	}
}

func scopedEngine(t *testing.T) (*Engine, stepper) {
	t.Helper()
	reg, err := NewRegistry(scopedChain())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return NewEngine(reg), newStepper(t)
}

func TestScopedDeskAdmitsOnlyTheOwner(t *testing.T) {
	e, step := scopedEngine(t)
	owner := ActorWithRole("alice@iag.local", "Project Manager")
	stranger := ActorWithRole("bob@iag.local", "Project Manager")

	s := New("scoped", requester.ID, Options{
		Scope: map[string]string{"project_owner": "alice@iag.local"},
	})
	s = step(e.Submit(s, requester))

	// Same role, different person: holds the role but not this request.
	if _, err := e.Advance(s, stranger, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a PM who does not own the project: %v, want ErrForbidden", err)
	}
	if e.Actionable(s, stranger) {
		t.Error("a non-owning PM must not see the request in their queue")
	}
	if !e.Actionable(s, owner) {
		t.Error("the assigned PM should see it")
	}
	s = step(e.Advance(s, owner, ""))
	if s.Desk != "head" {
		t.Fatalf("desk = %q, want head", s.Desk)
	}
}

func TestScopedDeskMatchesOnAnActorAttributeNotJustIdentity(t *testing.T) {
	e, step := scopedEngine(t)
	s := New("scoped", requester.ID, Options{
		Scope: map[string]string{"project_owner": "alice@iag.local", "department": "Ops"},
	})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, ActorWithRole("alice@iag.local", "Project Manager"), ""))

	// The Department Head desk is scoped to a group, not a person: the actor
	// matches through their own department attribute.
	wrongDept := Actor{ID: "carl@iag.local", Roles: []string{"Department Head"},
		Attrs: map[string]string{"department": "Logistics"}}
	if _, err := e.Advance(s, wrongDept, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("head of another department: %v, want ErrForbidden", err)
	}

	rightDept := Actor{ID: "dina@iag.local", Roles: []string{"Department Head"},
		Attrs: map[string]string{"department": "Ops"}}
	s = step(e.Advance(s, rightDept, ""))
	if s.Desk != "finance" {
		t.Fatalf("desk = %q, want finance", s.Desk)
	}
}

func TestUnscopedRequestFallsBackToRoleRatherThanStranding(t *testing.T) {
	e, step := scopedEngine(t)
	// No project owner: a requisition with no project still has to be approved
	// by somebody, so the desk stays role-wide rather than admitting nobody.
	s := New("scoped", requester.ID, Options{})
	s = step(e.Submit(s, requester))

	anyPM := ActorWithRole("bob@iag.local", "Project Manager")
	if !e.Actionable(s, anyPM) {
		t.Fatal("with no owner on the request the desk must fall back to the role")
	}
	s = step(e.Advance(s, anyPM, ""))
	if s.Desk != "head" {
		t.Fatalf("desk = %q, want head", s.Desk)
	}
}

func TestProgressShowsWhichDesksAreOwnerScoped(t *testing.T) {
	e, step := scopedEngine(t)
	s := New("scoped", requester.ID, Options{
		Scope: map[string]string{"project_owner": "alice@iag.local"},
	})
	s = step(e.Submit(s, requester))

	p, err := e.Progress(s)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	got := map[DeskKey]string{}
	for _, st := range p.Steps {
		got[st.Desk] = st.ScopedTo
	}
	if got["pm"] != "alice@iag.local" {
		t.Errorf("pm scopedTo = %q, want the project owner", got["pm"])
	}
	// Scoped by department, but this request carries none — the fallback applies
	// and the tracker must not claim a narrowing that is not in force.
	if got["head"] != "" {
		t.Errorf("head scopedTo = %q, want empty when the request has no department", got["head"])
	}
	if got["finance"] != "" {
		t.Errorf("finance scopedTo = %q, want empty on an unscoped desk", got["finance"])
	}
}

func TestAdminOverridesScope(t *testing.T) {
	e, step := scopedEngine(t)
	s := New("scoped", requester.ID, Options{
		Scope: map[string]string{"project_owner": "alice@iag.local"},
	})
	s = step(e.Submit(s, requester))

	s = step(e.Advance(s, admin, "PM unavailable"))
	if s.Desk != "head" {
		t.Fatalf("desk = %q, want head", s.Desk)
	}
	if !s.History[len(s.History)-1].Override {
		t.Error("an admin clearing a scoped desk must be recorded as an override")
	}
}

func TestScopeSurvivesCloneAndIsNotAliased(t *testing.T) {
	e, step := scopedEngine(t)
	scope := map[string]string{"project_owner": "alice@iag.local"}
	s := New("scoped", requester.ID, Options{Scope: scope})

	// Mutating the caller's map must not re-point the request's owner.
	scope["project_owner"] = "mallory@iag.local"
	s = step(e.Submit(s, requester))
	if s.Scope["project_owner"] != "alice@iag.local" {
		t.Fatalf("scope = %q; the state must not alias the caller's map", s.Scope["project_owner"])
	}
	if _, err := e.Advance(s, ActorWithRole("mallory@iag.local", "Project Manager"), ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("re-pointing the caller's map must not grant the desk")
	}
}

// sodChain is contract-management's shape: consecutive desks that one person
// must not walk alone, and a first desk the raiser themselves clears.
func sodChain() Chain {
	return Chain{
		Key: "sod", Label: "SoD", TerminalLabel: "Paid", NoSuccessiveApprover: true,
		Desks: []Desk{
			{Key: "pm", Label: "PM Approval", RolePatterns: []string{`.+`}, AllowRequester: true},
			{Key: "review", Label: "Finance Review", RolePatterns: []string{`.+`}, AllowRequester: true},
			{Key: "auth", Label: "Payment Authorization", RolePatterns: []string{`.+`}, AllowRequester: true},
		},
	}
}

func TestSuccessiveApproverIsBlocked(t *testing.T) {
	reg, err := NewRegistry(sodChain())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	alice := ActorWithRole("alice", "Officer")
	bob := ActorWithRole("bob", "Officer")

	s := step(e.Submit(New("sod", alice.ID, Options{}), alice))
	s = step(e.Advance(s, alice, ""))

	// Alice cleared the previous desk, so she cannot clear this one — even
	// though she holds the role and is allowed on the desk.
	if _, err := e.Advance(s, alice, ""); !errors.Is(err, ErrSelfSuccession) {
		t.Fatalf("same actor twice in a row: %v, want ErrSelfSuccession", err)
	}
	// Case-insensitively the same person.
	if _, err := e.Advance(s, ActorWithRole("Alice", "Officer"), ""); !errors.Is(err, ErrSelfSuccession) {
		t.Fatalf("same actor in different case: %v, want ErrSelfSuccession", err)
	}

	s = step(e.Advance(s, bob, ""))
	// Alice again is fine now: bob broke the succession.
	s = step(e.Advance(s, alice, ""))
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}
}

func TestSystemActorIsExemptFromSuccession(t *testing.T) {
	reg, err := NewRegistry(sodChain())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	system := ActorWithRole("system", "Automation")

	s := step(e.Submit(New("sod", system.ID, Options{}), system))
	for i := 0; i < 3; i++ {
		s = step(e.Advance(s, system, ""))
	}
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved — automation must not block itself", s.Status)
	}
}

func TestRepeatApproverIsBlockedAcrossTheWholeChain(t *testing.T) {
	chain := sodChain()
	chain.Key = "norepeat"
	chain.NoSuccessiveApprover = false
	chain.NoRepeatApprover = true
	reg, err := NewRegistry(chain)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	alice := ActorWithRole("alice", "Officer")
	bob := ActorWithRole("bob", "Officer")
	carol := ActorWithRole("carol", "Officer")

	s := step(e.Submit(New("norepeat", alice.ID, Options{}), alice))
	s = step(e.Advance(s, alice, ""))
	s = step(e.Advance(s, bob, ""))

	// Successive-only would allow alice here: bob broke the succession. A
	// repeat-approver rule does not, because she has already signed once.
	if _, err := e.Advance(s, alice, ""); !errors.Is(err, ErrRepeatApprover) {
		t.Fatalf("alice signing a second desk: %v, want ErrRepeatApprover", err)
	}
	s = step(e.Advance(s, carol, ""))
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}
}

func TestAmendmentClearsRepeatApproverHistory(t *testing.T) {
	chain := sodChain()
	chain.Key = "norepeat2"
	chain.NoSuccessiveApprover = false
	chain.NoRepeatApprover = true
	reg, err := NewRegistry(chain)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	alice := ActorWithRole("alice", "Officer")

	s := step(e.Submit(New("norepeat2", alice.ID, Options{}), alice))
	s = step(e.Advance(s, alice, ""))
	s = step(e.Amend(s, ActorWithRole("bob", "Officer"), "fix the figures"))
	s = step(e.Submit(s, alice))

	// The corrected request is a different one to approve; a signature on the
	// superseded version must not bar its author from the new walk.
	if _, err := e.Advance(s, alice, ""); err != nil {
		t.Fatalf("alice approving the amended request: %v, want allowed", err)
	}
}

func TestRepeatApproverExemptsSystemActors(t *testing.T) {
	chain := sodChain()
	chain.Key = "norepeat3"
	chain.NoSuccessiveApprover = false
	chain.NoRepeatApprover = true
	reg, err := NewRegistry(chain)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)
	system := ActorWithRole("system", "Automation")

	s := step(e.Submit(New("norepeat3", system.ID, Options{}), system))
	for i := 0; i < 3; i++ {
		s = step(e.Advance(s, system, ""))
	}
	if s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}
}

func TestSuccessionRuleIsOffByDefault(t *testing.T) {
	e, step := testEngine(t)
	// The requisition chain does not set NoSuccessiveApprover, so one person
	// holding two consecutive desks is allowed.
	both := Actor{ID: "u-multi", Roles: []string{"Project Manager", "Accounts Assistant"}}
	s := step(e.Submit(New("requisition", requester.ID, Options{Amount: 1_000_000}), requester))
	s = step(e.Advance(s, both, ""))
	if _, err := e.Advance(s, both, ""); err != nil {
		t.Fatalf("consecutive desks without the rule: %v, want allowed", err)
	}
}

func TestRebaseClosesTheBandBypass(t *testing.T) {
	e, step := testEngine(t)
	// Opened small, so GM and CEO never engaged.
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	if s.Desk != "accounts" {
		t.Fatalf("desk = %q, want accounts", s.Desk)
	}

	// The requisition is edited up to 25M while sitting on Accounts. Without a
	// rebase the request would finish at Finance with no GM or CEO signature.
	s = step(e.Rebase(s, 25_000_000))
	if s.Desk != "accounts" {
		t.Fatalf("rebase moved the request off its desk: %q", s.Desk)
	}

	s = step(e.Advance(s, accounts, ""))
	if s.Desk != "gm" {
		t.Fatalf("desk after accounts = %q, want gm — the raised amount must pull GM in", s.Desk)
	}
	s = step(e.Advance(s, gm, ""))
	if s.Desk != "ceo" {
		t.Fatalf("desk after gm = %q, want ceo", s.Desk)
	}
}

func TestRebaseRefusesWhenTheCurrentDeskFallsOutOfBand(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 25_000_000})
	s = step(e.Submit(s, requester))
	s = step(e.Advance(s, pm, ""))
	s = step(e.Advance(s, accounts, ""))
	if s.Desk != "gm" {
		t.Fatalf("desk = %q, want gm", s.Desk)
	}

	// Dropping the amount removes the GM desk the request is sitting on. There
	// is no honest place to put it, so it must go back for amendment.
	if _, err := e.Rebase(s, 100_000); !errors.Is(err, ErrAmountChanged) {
		t.Fatalf("rebase below the current desk's band: %v, want ErrAmountChanged", err)
	}
}

func TestRebaseIsANoOpWhenTheAmountIsUnchanged(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))
	before := s

	s = step(e.Rebase(s, 1_000_000))
	if s.StageIndex != before.StageIndex || s.Desk != before.Desk || len(s.History) != len(before.History) {
		t.Fatal("rebasing to the same amount must change nothing")
	}
}

func TestDeskRequiredPermIsEnforced(t *testing.T) {
	reg, err := NewRegistry(Chain{
		Key: "gated", Label: "Gated", TerminalLabel: "Approved",
		Desks: []Desk{{
			Key: "gm", Label: "General Manager", RolePatterns: []string{`\bgm\b`},
			RequiredPerm: "procurement.approve_requisition_tier2",
		}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, step := NewEngine(reg), newStepper(t)

	open := func() State {
		return step(e.Submit(New("gated", requester.ID, Options{}), requester))
	}

	// Holds the desk by role but carries no permissions.
	without := Actor{ID: "u-gm", Roles: []string{"GM"}, HasPerm: func(string) bool { return false }}
	if _, err := e.Advance(open(), without, ""); !errors.Is(err, ErrMissingPermission) {
		t.Fatalf("advance without the desk permission: %v, want ErrMissingPermission", err)
	}

	with := Actor{ID: "u-gm", Roles: []string{"GM"},
		HasPerm: func(code string) bool { return code == "procurement.approve_requisition_tier2" }}
	if s := step(e.Advance(open(), with, "")); s.Status != StatusApproved {
		t.Fatalf("status = %s, want approved", s.Status)
	}

	// A nil oracle opts out of permission gating entirely.
	if s := step(e.Advance(open(), Actor{ID: "u-gm", Roles: []string{"GM"}}, "")); s.Status != StatusApproved {
		t.Fatal("a nil HasPerm should leave role matching as the only gate")
	}

	// An admin still passes the gate, and the step is flagged as an override.
	adm := Actor{ID: "u-adm", Roles: []string{"Administrator"}, Admin: true,
		HasPerm: func(string) bool { return false }}
	s := step(e.Advance(open(), adm, ""))
	if !s.History[len(s.History)-1].Override {
		t.Fatal("an admin clearing a permission gate must be recorded as an override")
	}
}

func deskKeys(ds []Desk) []DeskKey {
	out := make([]DeskKey, len(ds))
	for i, d := range ds {
		out[i] = d.Key
	}
	return out
}

// Cancelling your own request is a withdrawal and needs no justification.
// Cancelling somebody else's is an administrative override, and the record has
// to say why — otherwise the audit trail shows an administrator ending a
// request and is silent on the reason, which is the first thing anyone asks.
// Reject and amend have always demanded a reason; cancel was the way round it.

func TestAdminCancellingSomeoneElsesRequestMustSayWhy(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))

	if _, err := e.Cancel(s, admin, ""); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("admin cancelled a stranger's request with no reason: %v, want ErrReasonRequired", err)
	}
	if _, err := e.Cancel(s, admin, "   "); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("blank reason accepted: %v, want ErrReasonRequired", err)
	}

	out := step(e.Cancel(s, admin, "duplicate of REQ-1041"))
	if out.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", out.Status)
	}
	last := out.History[len(out.History)-1]
	if last.Reason != "duplicate of REQ-1041" {
		t.Errorf("reason = %q, want it recorded verbatim", last.Reason)
	}
	if !last.Override {
		t.Error("an admin cancelling another person's request must be stamped as an override")
	}
	if last.Actor != admin.ID {
		t.Errorf("actor = %q, want the administrator who did it", last.Actor)
	}
}

// The requester's own withdrawal stays free-form, so the fix does not put
// friction on the ordinary path.
func TestRequesterMayWithdrawWithoutGivingAReason(t *testing.T) {
	e, step := testEngine(t)
	s := New("requisition", requester.ID, Options{Amount: 1_000_000})
	s = step(e.Submit(s, requester))

	out, err := e.Cancel(s, requester, "")
	if err != nil {
		t.Fatalf("withdrawing own request: %v, want it allowed", err)
	}
	if out.Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", out.Status)
	}
	if out.History[len(out.History)-1].Override {
		t.Error("a requester withdrawing their own request is not an override")
	}
}
