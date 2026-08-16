package approvalchain

import "strings"

// Matches reports whether a role name holds this desk.
//
// Matching is by regular expression rather than equality because role names are
// deployment data, not code: the same desk is "GM" in one tenant and "General
// Manager" in another. Patterns are compiled case-insensitively at registration.
func (d Desk) Matches(role string) bool {
	role = strings.TrimSpace(role)
	if role == "" {
		return false
	}
	for _, re := range d.roles {
		if re.MatchString(role) {
			return true
		}
	}
	return false
}

// MatchedRole returns the actor's first role that holds this desk. The matched
// role — not the whole list — is what history records, so an audit reads "acted
// as General Manager" rather than naming every hat the person wears.
func (d Desk) MatchedRole(a Actor) (string, bool) {
	for _, r := range a.Roles {
		if d.Matches(r) {
			return strings.TrimSpace(r), true
		}
	}
	return "", false
}

// Holds reports whether the actor may act on this desk by role alone, ignoring
// any scope. It is the coarse filter behind a desk queue's SQL prefilter, where
// per-request scope is not yet known; HoldsFor is the precise check.
func (d Desk) Holds(a Actor) bool {
	if _, ok := d.MatchedRole(a); ok {
		return true
	}
	return a.Admin
}

// HoldsFor reports whether the actor may act on this desk for a request with
// the given scope — role first, then ownership.
//
// An admin passes regardless, and the step records the override.
func (d Desk) HoldsFor(a Actor, scope map[string]string) bool {
	if a.Admin {
		return true
	}
	if !d.Holds(a) {
		return false
	}
	return d.inScope(a, scope)
}

// inScope applies ScopeBy. A request with no value for the key is not narrowed:
// scope data is often optional, and stranding those requests with nobody able
// to approve them would be worse than falling back to the role.
func (d Desk) inScope(a Actor, scope map[string]string) bool {
	key := strings.TrimSpace(d.ScopeBy)
	if key == "" {
		return true
	}
	want := strings.TrimSpace(scope[key])
	if want == "" {
		return true
	}
	if strings.EqualFold(want, strings.TrimSpace(a.ID)) {
		return true
	}
	return strings.EqualFold(want, strings.TrimSpace(a.Attrs[key]))
}

// Scoped reports whether this desk narrows by ownership for the given request.
// Progress uses it so a reader can tell a desk anyone with the role may clear
// from one only its owner may.
func (d Desk) Scoped(scope map[string]string) bool {
	key := strings.TrimSpace(d.ScopeBy)
	return key != "" && strings.TrimSpace(scope[key]) != ""
}

// HasPerm is how an actor proves it carries a permission code. Services plug in
// their own permission model; the engine only asks the question.
type HasPerm func(code string) bool

// checkPerm enforces the desk's RequiredPerm against the actor.
//
// Holding a desk by role is not sufficient on its own: the desk matrix declares
// a permission per desk, and a matrix that advertises a gate nothing checks is
// worse than no gate at all. An admin still passes — the override is recorded.
func (d Desk) checkPerm(a Actor) error {
	if strings.TrimSpace(d.RequiredPerm) == "" || a.Admin {
		return nil
	}
	if a.HasPerm == nil {
		// No permission oracle supplied: the caller has opted out of permission
		// gating, so role matching stands alone.
		return nil
	}
	if !a.HasPerm(d.RequiredPerm) {
		return ErrMissingPermission
	}
	return nil
}

// deskHeldByRole reports whether the actor holds the desk on merit rather than
// on admin rights. It is what decides whether a step is flagged as an override.
func deskHeldByRole(d Desk, a Actor) bool {
	_, ok := d.MatchedRole(a)
	return ok
}

// byRole is the role recorded on a step: the matching one where there is one,
// otherwise the actor's first role, so an admin override still says who acted.
func (d Desk) byRole(a Actor) string {
	if r, ok := d.MatchedRole(a); ok {
		return r
	}
	if len(a.Roles) > 0 {
		return strings.TrimSpace(a.Roles[0])
	}
	return ""
}

// DesksForRole lists the desks in a chain a role can act on. Used to build the
// "what is waiting on me" query without scanning every open request.
func (c *Chain) DesksForRole(role string) []DeskKey {
	out := make([]DeskKey, 0, len(c.Desks))
	for _, d := range c.Desks {
		if d.Matches(role) {
			out = append(out, d.Key)
		}
	}
	return out
}

// DesksForActor lists the desks in a chain any of the actor's roles can act on.
func (c *Chain) DesksForActor(a Actor) []DeskKey {
	out := make([]DeskKey, 0, len(c.Desks))
	for _, d := range c.Desks {
		if d.Holds(a) {
			out = append(out, d.Key)
		}
	}
	return out
}

// DesksForRole lists desks across every registered chain a role can act on,
// keyed by chain. A desk queue query is: for each chain, any open request whose
// current desk is in this set.
func (r *Registry) DesksForRole(role string) map[string][]DeskKey {
	out := make(map[string][]DeskKey, len(r.order))
	for _, key := range r.order {
		if desks := r.chains[key].DesksForRole(role); len(desks) > 0 {
			out[key] = desks
		}
	}
	return out
}

// DesksForActor is DesksForRole across all of an actor's roles. An admin holds
// every desk, which is what makes an admin desk queue show the whole backlog.
func (r *Registry) DesksForActor(a Actor) map[string][]DeskKey {
	out := make(map[string][]DeskKey, len(r.order))
	for _, key := range r.order {
		if desks := r.chains[key].DesksForActor(a); len(desks) > 0 {
			out[key] = desks
		}
	}
	return out
}
