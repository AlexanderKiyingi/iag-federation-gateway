package ratelimit

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ApplyTrustedProxies configures gin so c.ClientIP() derives the real client IP
// from X-Forwarded-For ONLY when the immediate peer is one of the trusted
// proxies, and otherwise uses the direct socket peer.
//
// This closes the common gap where a service left at gin's default trusts EVERY
// proxy, letting any client forge X-Forwarded-For to evade an IP-keyed limit or
// to pin a victim's IP. Pass the CIDRs/IPs of your edge/gateway hop(s).
//
// An empty list trusts NO proxy: c.ClientIP() becomes the direct peer — safe
// from spoofing, but behind an edge every request then looks like the edge, so
// per-IP limiting collapses to one bucket. Behind the platform gateway, pass the
// gateway/edge CIDR so real client IPs survive.
func ApplyTrustedProxies(engine *gin.Engine, trusted []string) error {
	if len(trusted) == 0 {
		return engine.SetTrustedProxies(nil)
	}
	return engine.SetTrustedProxies(trusted)
}

// ParseTrustedProxies parses a comma/space-separated env value into a proxy
// list. "none" (case-insensitive) or empty yields nil (trust no proxy).
func ParseTrustedProxies(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
