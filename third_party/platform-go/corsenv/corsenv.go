package corsenv

import (
	"os"
	"strings"
)

// EnvKeys lists supported environment variable names for browser CORS allowlists,
// in priority order. CORS_ALLOWED_ORIGINS is the canonical platform name.
var EnvKeys = []string{
	"CORS_ALLOWED_ORIGINS",
	"CORS_ALLOW_ORIGIN",
	"CORS_ORIGIN",
	"ALLOWED_ORIGINS",
	"CORS_ORIGINS",
}

const DefaultDevOrigins = "http://localhost:3000,http://localhost:5173"

// Allowlist returns the first non-empty CORS allowlist from env, or fallback.
func Allowlist(fallback string) string {
	for _, key := range EnvKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if fallback == "" {
		return DefaultDevOrigins
	}
	return fallback
}

// HasWildcard reports whether the allowlist includes "*".
func HasWildcard(allowed string) bool {
	if allowed == "*" {
		return true
	}
	for _, o := range strings.Split(allowed, ",") {
		if strings.TrimSpace(o) == "*" {
			return true
		}
	}
	return false
}
