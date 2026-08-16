// Package approvalchain models desk-based approval: a request walks an ordered
// sequence of named desks, each held by a role, before it is approved.
//
// It exists because the platform had grown five divergent approval mechanisms —
// amount-band tiers in finance and procurement, hardcoded stage arrays in
// contract-management, ad-hoc approve/reject in project-management, and
// per-entity stage advance in fleet. None of them modelled the thing the
// business actually does: route a request through named desks (Project Manager,
// Accounts Assistant, GM, CEO, Finance), let each desk approve, reject with a
// reason, or return it for amendment, and show every holder of a role what is
// waiting on them.
//
// Desks and amount bands compose rather than compete. A chain lists its desks in
// order; each desk may carry a MinAmount, so a small requisition stops at
// Accounts while a large one continues to GM and CEO. The band decides *whether*
// a desk engages; the chain decides *when*.
//
// The engine is pure: it owns no storage and no transport. Callers persist
// State however they like and pass it back in. See engine.go for transitions and
// queue.go for the desk queue.
package approvalchain

import (
	"fmt"
	"regexp"

	"strings"
)

// DeskKey identifies a desk within a chain (e.g. "gm", "ceo", "finance").
type DeskKey string

// Desk is one approval station in a chain.
type Desk struct {
	// Key identifies the desk within its chain and is what State records.
	Key DeskKey
	// Label is the human name of the desk ("General Manager").
	Label string
	// RolePatterns are case-insensitive regular expressions matched against an
	// actor's role. Role naming varies across deployments ("GM", "General
	// Manager", "Gen. Manager"), so matching is deliberately tolerant.
	RolePatterns []string
	// MinAmount gates the desk: it engages only when the request amount is at
	// or above this value. Zero means the desk always engages.
	MinAmount float64
	// RequiredPerm, when set, is a permission the caller should also check.
	// The engine records it on the step but does not enforce it — permission
	// systems differ per service.
	RequiredPerm string
	// ActionLabel is what this desk's approve button says ("Approve", "Make
	// payment", "Issue materials"). Defaults to "Approve".
	ActionLabel string
	// StatusLabel is the human status a request carries once this desk has
	// passed it ("GM Approved"). Defaults to Label + " Approved".
	StatusLabel string
	// AllowRequester lets the requester act on this desk. Off by default: the
	// four-eyes rule is that you cannot approve your own request.
	AllowRequester bool
	// ScopeBy narrows the desk from "anyone with the role" to "the person or
	// group this request belongs to" — the difference between any Project
	// Manager approving any request, and the assigned PM approving theirs.
	//
	// It names a key in State.Scope. The actor matches when their ID equals the
	// request's value for that key, or when one of their Attrs under the same
	// key does. Empty leaves the desk role-global.
	//
	// A request that carries no value for the key is not narrowed: scope data is
	// often optional (a requisition with no project has no project owner), and
	// stranding those requests with nobody able to approve them would be worse
	// than falling back to the role. Progress reports which desks were scoped so
	// the fallback is visible rather than silent.
	ScopeBy string

	roles []*regexp.Regexp
}

// Chain is an ordered set of desks a request walks.
type Chain struct {
	// Key identifies the chain ("requisition", "material.stores").
	Key string
	// Label is the human summary ("Requestor → PM → Accounts → GM → CEO → Finance → Paid").
	Label string
	// Desks are walked in slice order.
	Desks []Desk
	// TerminalLabel is the status once every engaged desk has passed ("Paid").
	// Defaults to "Approved".
	TerminalLabel string
	// AllowAutoApprove permits a chain whose engaged desk set can be empty —
	// a request below every band is approved on submit. Off by default so a
	// mis-typed band cannot silently create an unapproved approval.
	AllowAutoApprove bool
	// NoSuccessiveApprover blocks the actor who cleared the previous desk from
	// clearing this one — segregation of duties along the chain, rather than
	// only against the requester.
	//
	// It is the stronger control: four-eyes on the requester alone still lets
	// one approver walk a request through every desk. Chains where the same
	// person legitimately holds consecutive desks should leave it off.
	//
	// System actors (empty or "system") are exempt, so automated advances are
	// not blocked by a rule about people.
	NoSuccessiveApprover bool
	// NoRepeatApprover blocks anyone who has already cleared a desk on this
	// request from clearing another — one person, at most one signature.
	//
	// Stronger than NoSuccessiveApprover, which only separates neighbours: with
	// successive-only, an approver holding desks one and three still signs a
	// request twice. Chains that replace an amount-band tier matrix want this
	// one, because tiers are normally counted as distinct signatures.
	//
	// System actors are exempt for the same reason as above.
	NoRepeatApprover bool
}

// Options describe one request's shape, and decide which desks engage.
type Options struct {
	// Amount is compared against each desk's MinAmount.
	Amount float64
	// Skip names desks that do not apply to this request regardless of amount —
	// how "oral, general, fleet and payroll requests skip the PM desk" is
	// expressed without duplicating the chain.
	Skip []DeskKey
	// Scope is who the request belongs to, keyed by the names desks use in
	// ScopeBy. See Desk.ScopeBy.
	Scope map[string]string
}

func (o Options) skipped(key DeskKey) bool {
	for _, k := range o.Skip {
		if k == key {
			return true
		}
	}
	return false
}

// amountEpsilon guards band comparisons against float rounding. Amounts are
// stored as NUMERIC and compared here as float64; without it a 5_000_000.0000001
// total could fall on the wrong side of a 5_000_000 band.
const amountEpsilon = 0.005

// Engaged returns the desks this request must actually pass, in order.
func (c *Chain) Engaged(opt Options) []Desk {
	out := make([]Desk, 0, len(c.Desks))
	for _, d := range c.Desks {
		if opt.skipped(d.Key) {
			continue
		}
		if d.MinAmount > 0 && opt.Amount < d.MinAmount-amountEpsilon {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Desk returns the desk with the given key.
func (c *Chain) Desk(key DeskKey) (Desk, bool) {
	for _, d := range c.Desks {
		if d.Key == key {
			return d, true
		}
	}
	return Desk{}, false
}

// Terminal is the status label once the chain completes.
func (c *Chain) Terminal() string {
	if strings.TrimSpace(c.TerminalLabel) != "" {
		return c.TerminalLabel
	}
	return "Approved"
}

// Action is what this desk's approve control should say.
func (d Desk) Action() string {
	if strings.TrimSpace(d.ActionLabel) != "" {
		return d.ActionLabel
	}
	return "Approve"
}

// PassedStatus is the status a request carries once this desk has passed it.
func (d Desk) PassedStatus() string {
	if strings.TrimSpace(d.StatusLabel) != "" {
		return d.StatusLabel
	}
	return d.Label + " Approved"
}

// Registry holds the chains a service knows about.
type Registry struct {
	chains map[string]*Chain
	order  []string
}

// NewRegistry compiles and validates chains. It fails rather than silently
// accepting a chain that cannot route — a bad definition is a deployment bug,
// not a runtime condition.
func NewRegistry(chains ...Chain) (*Registry, error) {
	r := &Registry{chains: make(map[string]*Chain, len(chains))}
	for i := range chains {
		c := chains[i]
		if err := compile(&c); err != nil {
			return nil, fmt.Errorf("chain %q: %w", c.Key, err)
		}
		if _, dup := r.chains[c.Key]; dup {
			return nil, fmt.Errorf("chain %q: declared twice", c.Key)
		}
		r.chains[c.Key] = &c
		r.order = append(r.order, c.Key)
	}
	return r, nil
}

// MustRegistry is NewRegistry for package-level definitions, where a bad chain
// should stop the process at startup.
func MustRegistry(chains ...Chain) *Registry {
	r, err := NewRegistry(chains...)
	if err != nil {
		panic("approvalchain: " + err.Error())
	}
	return r
}

// Get returns a chain by key.
func (r *Registry) Get(key string) (*Chain, bool) {
	c, ok := r.chains[strings.TrimSpace(key)]
	return c, ok
}

// Keys lists registered chain keys in declaration order.
func (r *Registry) Keys() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Meta describes every chain for clients that render trackers and desk filters,
// so the UI never has to hardcode a stage list.
func (r *Registry) Meta() []ChainMeta {
	out := make([]ChainMeta, 0, len(r.order))
	for _, key := range r.order {
		c := r.chains[key]
		desks := make([]DeskMeta, 0, len(c.Desks))
		for _, d := range c.Desks {
			desks = append(desks, DeskMeta{
				Key:          d.Key,
				Label:        d.Label,
				MinAmount:    d.MinAmount,
				RequiredPerm: d.RequiredPerm,
				ActionLabel:  d.Action(),
				StatusLabel:  d.PassedStatus(),
			})
		}
		out = append(out, ChainMeta{
			Key: c.Key, Label: c.Label, Terminal: c.Terminal(), Desks: desks,
		})
	}
	return out
}

// ChainMeta is the wire description of a chain.
type ChainMeta struct {
	Key      string     `json:"key"`
	Label    string     `json:"label"`
	Terminal string     `json:"terminal"`
	Desks    []DeskMeta `json:"desks"`
}

// DeskMeta is the wire description of a desk.
type DeskMeta struct {
	Key          DeskKey `json:"key"`
	Label        string  `json:"label"`
	MinAmount    float64 `json:"minAmount,omitempty"`
	RequiredPerm string  `json:"requiredPerm,omitempty"`
	ActionLabel  string  `json:"actionLabel"`
	StatusLabel  string  `json:"statusLabel"`
}

func compile(c *Chain) error {
	if strings.TrimSpace(c.Key) == "" {
		return errBlankChainKey
	}
	if len(c.Desks) == 0 {
		return errNoDesks
	}
	seen := make(map[DeskKey]struct{}, len(c.Desks))
	unbanded := false
	for i := range c.Desks {
		d := &c.Desks[i]
		if strings.TrimSpace(string(d.Key)) == "" {
			return fmt.Errorf("desk %d: %w", i, errBlankDeskKey)
		}
		if _, dup := seen[d.Key]; dup {
			return fmt.Errorf("desk %q: %w", d.Key, errDuplicateDesk)
		}
		seen[d.Key] = struct{}{}
		if strings.TrimSpace(d.Label) == "" {
			d.Label = string(d.Key)
		}
		if len(d.RolePatterns) == 0 {
			return fmt.Errorf("desk %q: %w", d.Key, errNoRoles)
		}
		d.roles = d.roles[:0]
		for _, p := range d.RolePatterns {
			re, err := regexp.Compile("(?i)" + p)
			if err != nil {
				return fmt.Errorf("desk %q: role pattern %q: %w", d.Key, p, err)
			}
			d.roles = append(d.roles, re)
		}
		if d.MinAmount < 0 {
			return fmt.Errorf("desk %q: %w", d.Key, errNegativeBand)
		}
		if d.MinAmount == 0 {
			unbanded = true
		}
	}
	// Bands must not be able to empty the chain: without at least one desk that
	// always engages, a request below every band would approve itself.
	if !unbanded && !c.AllowAutoApprove {
		return errAllDesksBanded
	}
	// Bands deliberately need not increase along the chain: a desk that always
	// engages can legitimately sit after banded ones — Finance still pays a
	// small requisition that never reached GM or CEO.
	return nil
}
