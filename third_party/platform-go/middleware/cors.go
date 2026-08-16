package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORS reflects allowed browser origins from a comma-separated allowlist.
func CORS(allowed string) gin.HandlerFunc {
	allowAny := allowed == "*"
	allowedOrigins := splitAllowedOrigins(allowed)
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if allowAny || (origin != "" && originAllowed(origin, allowedOrigins)) {
			if origin != "" {
				c.Header("Access-Control-Allow-Origin", origin)
			} else if allowAny {
				c.Header("Access-Control-Allow-Origin", "*")
			}
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, If-Match, X-Request-ID, X-Workspace-User")
		c.Header("Access-Control-Expose-Headers", "ETag, X-Request-ID")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func splitAllowedOrigins(allowed string) []string {
	if allowed == "" || allowed == "*" {
		return nil
	}
	parts := strings.Split(allowed, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func originAllowed(origin string, allowed []string) bool {
	for _, candidate := range allowed {
		if origin == candidate {
			return true
		}
	}
	return false
}
