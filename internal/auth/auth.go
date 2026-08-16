// Package auth implements per-route permission checks. The middleware verifies
// the inbound JWT and attaches claims; this package only inspects those claims.
package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/alvor-technologies/iag-platform-go/apierr"
	"github.com/iag/federation-gateway/internal/ctxkeys"
	"github.com/iag/federation-gateway/internal/middleware"
)

// StrictRBAC enables fail-closed permission checks when JWT permission lists
// are empty (production default).
func StrictRBAC() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ctxkeys.StrictRBAC, true)
		c.Next()
	}
}

func isStrictRBAC(c *gin.Context) bool {
	v, ok := c.Get(ctxkeys.StrictRBAC)
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// HasPerm reports whether the current principal carries the named permission.
// Superusers and staff always pass; a "*" wildcard permission also passes. When
// strict RBAC is off (dev), an empty permission list is treated as allow.
func HasPerm(c *gin.Context, codename string) bool {
	claims, ok := middleware.Claims(c)
	if !ok || claims == nil {
		return false
	}
	if claims.IsSuperuser || claims.IsStaff {
		return true
	}
	if len(claims.Permissions) == 0 {
		return !isStrictRBAC(c)
	}
	for _, p := range claims.Permissions {
		if p == "*" || p == codename {
			return true
		}
	}
	return false
}

func RequirePerm(codename string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := middleware.Claims(c); !ok {
			apierr.Unauthorized(c, "authentication required")
			return
		}
		if !HasPerm(c, codename) {
			apierr.WriteWith(c, http.StatusForbidden, apierr.CodeForbidden,
				"permission denied: "+codename, gin.H{"required_permission": codename})
			return
		}
		c.Next()
	}
}

func RequireStaff() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := middleware.Claims(c)
		if !ok || claims == nil {
			apierr.Unauthorized(c, "authentication required")
			return
		}
		if !claims.IsStaff && !claims.IsSuperuser {
			apierr.Forbidden(c, "staff access required")
			return
		}
		c.Next()
	}
}

// ActorName re-exports the middleware helper for handler convenience.
func ActorName(c *gin.Context) string { return middleware.ActorName(c) }
