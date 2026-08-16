// Package middleware implements Bearer+aud authentication for inbound federation-gateway
// requests. Every request under /v1 must carry a verifiable JWT with
// aud=iag.federation-gateway; probe and root paths are public.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/alvor-technologies/iag-platform-go/apierr"
	"github.com/iag/federation-gateway/internal/platformauth"
)

const claimsKey = "iag.claims"

type PlatformAuth struct {
	verifier *platformauth.Verifier
}

func NewPlatformAuth(verifier *platformauth.Verifier) *PlatformAuth {
	return &PlatformAuth{verifier: verifier}
}

func isPublicProbePath(path string) bool {
	switch path {
	case "/", "/health", "/healthz", "/ready":
		return true
	default:
		return false
	}
}

// AttachPrincipal verifies the inbound token and stores the claims on the
// context. Public probe paths pass through untouched.
func (m *PlatformAuth) AttachPrincipal() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isPublicProbePath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if m.verifier == nil {
			apierr.Write(c, http.StatusServiceUnavailable, apierr.CodeServiceUnavailable, "JWT verifier not configured")
			return
		}
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			apierr.Unauthorized(c, "missing bearer token")
			return
		}
		claims, err := m.verifier.Verify(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			apierr.Unauthorized(c, "invalid or expired token")
			return
		}
		c.Set(claimsKey, claims)
		c.Next()
	}
}

// Claims returns the verified claims stored on the context.
func Claims(c *gin.Context) (*platformauth.Claims, bool) {
	v, ok := c.Get(claimsKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*platformauth.Claims)
	return claims, ok
}

// ActorName resolves a human-readable actor label for audit fields.
func ActorName(c *gin.Context) string {
	claims, ok := Claims(c)
	if !ok || claims == nil {
		return "anonymous"
	}
	switch {
	case claims.Name != "":
		return claims.Name
	case claims.Email != "":
		return claims.Email
	case claims.ClientID != "":
		return "client:" + claims.ClientID
	case claims.Subject != "":
		return claims.Subject
	default:
		return "unknown"
	}
}
