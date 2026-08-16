// Package middleware provides Gin middleware for IAG services.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/alvor-technologies/iag-platform-go/apierr"
	"github.com/alvor-technologies/iag-platform-go/authclient"
)

const (
	// CtxClaims is the gin.Context key holding the verified *authclient.Claims.
	CtxClaims = "iag.claims"
	// CtxUserID is the gin.Context key holding the parsed user uuid.UUID (zero for service principals).
	CtxUserID = "iag.userID"
	// CtxPrincipalID is the gin.Context key holding the principal identifier:
	// either a user UUID or "client:<id>" for service principals.
	CtxPrincipalID = "iag.principalID"
)

// Principal verifies the Authorization: Bearer <token> header against the
// supplied Verifier and stores the claims on the request context. Tokens that
// fail verification (signature, issuer, audience, expiry) cause 401.
func Principal(v *authclient.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			apierr.Unauthorized(c, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		claims, err := v.Verify(token)
		if err != nil {
			apierr.Unauthorized(c, "invalid or expired token")
			return
		}
		c.Set(CtxClaims, claims)
		c.Set(CtxPrincipalID, claims.Subject)
		if claims.IsUser() {
			if uid, err := uuid.Parse(claims.Subject); err == nil {
				c.Set(CtxUserID, uid)
			}
		}
		c.Next()
	}
}

// RequirePermission aborts the request unless the verified principal carries
// one of the supplied permissions. Superusers always pass.
func RequirePermission(perms ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok {
			apierr.Unauthorized(c, "authentication required")
			return
		}
		if !claims.HasAnyPermission(perms...) {
			apierr.WriteWith(c, http.StatusForbidden, apierr.CodeForbidden,
				"permission denied", gin.H{"required_permission": perms})
			return
		}
		c.Next()
	}
}

// RequireService aborts the request unless the verified principal is a service-account.
// Use for endpoints exposed solely to other backend services.
func RequireService() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok || !claims.IsService() {
			apierr.Forbidden(c, "service principal required")
			return
		}
		c.Next()
	}
}

// RequireUser aborts the request unless the verified principal is a human user.
func RequireUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok || !claims.IsUser() {
			apierr.Forbidden(c, "user principal required")
			return
		}
		c.Next()
	}
}

// ClaimsFrom returns the verified claims stored on the gin context.
func ClaimsFrom(c *gin.Context) (*authclient.Claims, bool) {
	v, ok := c.Get(CtxClaims)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*authclient.Claims)
	return claims, ok
}

// UserIDFrom returns the parsed user UUID (zero + false for service principals).
func UserIDFrom(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(CtxUserID)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

// PrincipalIDFrom returns the principal identifier (uuid for users, client:<id> for services).
func PrincipalIDFrom(c *gin.Context) string {
	v, ok := c.Get(CtxPrincipalID)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
