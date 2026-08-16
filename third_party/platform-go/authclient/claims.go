package authclient

import (
	"github.com/golang-jwt/jwt/v5"
)

// PrincipalType differentiates a human user from a service-account caller.
type PrincipalType string

const (
	PrincipalUser    PrincipalType = "user"
	PrincipalService PrincipalType = "service"
)

// Claims is the canonical IAG JWT payload. Every service that verifies tokens
// reads these fields — keep changes additive.
type Claims struct {
	Email         string        `json:"email,omitempty"`
	Name          string        `json:"name,omitempty"`
	PrincipalType PrincipalType `json:"principal_type"`
	ClientID      string        `json:"client_id,omitempty"`
	IsSuperuser   bool          `json:"is_superuser,omitempty"`
	IsStaff       bool          `json:"is_staff,omitempty"`
	Groups        []string      `json:"groups,omitempty"`
	Permissions   []string      `json:"permissions,omitempty"`
	jwt.RegisteredClaims
}

// IsService reports whether the token represents a service-account caller.
func (c *Claims) IsService() bool { return c.PrincipalType == PrincipalService }

// IsUser reports whether the token represents a human user.
func (c *Claims) IsUser() bool { return c.PrincipalType == PrincipalUser || c.PrincipalType == "" }

// HasPermission reports whether the principal carries the named permission.
// Superusers implicitly pass any permission check.
func (c *Claims) HasPermission(name string) bool {
	if c.IsSuperuser {
		return true
	}
	for _, p := range c.Permissions {
		if p == name {
			return true
		}
	}
	return false
}

// HasAnyPermission reports whether the principal carries any of the named permissions.
func (c *Claims) HasAnyPermission(names ...string) bool {
	if c.IsSuperuser {
		return true
	}
	if len(names) == 0 {
		return true
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	for _, p := range c.Permissions {
		if _, ok := want[p]; ok {
			return true
		}
	}
	return false
}

// InGroup reports whether the principal belongs to the named group.
func (c *Claims) InGroup(name string) bool {
	for _, g := range c.Groups {
		if g == name {
			return true
		}
	}
	return false
}
