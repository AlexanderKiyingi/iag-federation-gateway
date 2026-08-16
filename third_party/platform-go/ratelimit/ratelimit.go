// Package ratelimit is the platform's shared HTTP rate-limiting middleware for
// gin services. It exists so every service throttles the same way instead of
// hand-rolling a limiter: correct client-IP handling (via trusted proxies), a
// pluggable Store (in-memory by default, or a distributed backend a service
// injects), an explicit fail-open/fail-closed policy, and standard 429 +
// Retry-After responses with hooks for metrics/alerting.
//
// Typical wiring in a service:
//
//	ratelimit.ApplyTrustedProxies(engine, cfg.TrustedProxies) // don't trust every hop
//	r.Use(ratelimit.Middleware(ratelimit.Config{
//	    Rate: ratelimit.PerMinute(300),           // per-key budget
//	    Key:  ratelimit.ByUserOrIP,               // authed → per user, else per IP
//	}))
//
// The default Store is in-memory (per process). A horizontally-scaled service
// should inject a shared Store (e.g. Redis) so the limit is global.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/alvor-technologies/iag-platform-go/apierr"
	"github.com/alvor-technologies/iag-platform-go/middleware"
	"github.com/gin-gonic/gin"
)

// Rate is a request budget: at most Requests per Window.
type Rate struct {
	Requests int
	Window   time.Duration
}

// PerMinute is the common case: n requests per rolling minute.
func PerMinute(n int) Rate { return Rate{Requests: n, Window: time.Minute} }

// Valid reports whether the rate is usable (positive budget and window).
func (r Rate) Valid() bool { return r.Requests > 0 && r.Window > 0 }

// Store decides whether a key may proceed under the given Rate. Implementations
// may be in-memory (per process) or distributed. Allow returns whether the
// request is allowed, how long to wait before retrying when it is not, and an
// error if the backing store itself failed (the middleware then applies its
// fail-open / fail-closed policy).
type Store interface {
	Allow(ctx context.Context, key string, rate Rate) (allowed bool, retryAfter time.Duration, err error)
}

// KeyFunc derives the bucket key from a request. Returning "" skips limiting for
// that request (e.g. an unauthenticated context you deliberately don't throttle).
type KeyFunc func(c *gin.Context) string

// Config configures Middleware.
type Config struct {
	// Rate is the per-key budget. Required (an invalid Rate disables limiting).
	Rate Rate
	// Key derives the bucket key. Defaults to ByIP.
	Key KeyFunc
	// Store backs the counters. Defaults to a process-local in-memory store.
	Store Store
	// FailClosed denies requests (503) when the Store errors. Default is
	// fail-open (allow) so a store outage degrades to no-limiting rather than a
	// full outage — flip on for security-critical surfaces.
	FailClosed bool
	// OnError is invoked when the Store errors (logging / metrics). Optional.
	OnError func(c *gin.Context, err error)
	// OnThrottle is invoked when a request is rejected with 429 (metrics /
	// alerting so an attack in progress is observable). Optional.
	OnThrottle func(c *gin.Context, key string)
}

// Middleware returns a gin handler that enforces cfg. Safe defaults are applied
// for an omitted Key (ByIP) and Store (in-memory). An invalid Rate makes the
// middleware a no-op (so a misconfiguration fails open rather than blocking all
// traffic).
func Middleware(cfg Config) gin.HandlerFunc {
	key := cfg.Key
	if key == nil {
		key = ByIP
	}
	store := cfg.Store
	if store == nil {
		store = NewMemoryStore()
	}
	rate := cfg.Rate
	return func(c *gin.Context) {
		if !rate.Valid() {
			c.Next()
			return
		}
		k := key(c)
		if k == "" {
			c.Next()
			return
		}
		allowed, retryAfter, err := store.Allow(c.Request.Context(), k, rate)
		if err != nil {
			if cfg.OnError != nil {
				cfg.OnError(c, err)
			}
			if !cfg.FailClosed {
				c.Next()
				return
			}
			c.Header("Retry-After", "1")
			apierr.JSONStatus(c, http.StatusServiceUnavailable, "rate limiter unavailable")
			c.Abort()
			return
		}
		if !allowed {
			if cfg.OnThrottle != nil {
				cfg.OnThrottle(c, k)
			}
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter, rate)))
			apierr.Write(c, http.StatusTooManyRequests, apierr.CodeTooManyRequests, "rate limit exceeded")
			c.Abort()
			return
		}
		c.Next()
	}
}

// retryAfterSeconds converts a refill delay to a whole-second Retry-After,
// clamped to [1, window] so the hint is meaningful rather than a static 60.
func retryAfterSeconds(d time.Duration, rate Rate) int {
	secs := int(d.Seconds())
	if d > 0 && time.Duration(secs)*time.Second < d {
		secs++ // round up so the client actually waits long enough
	}
	if secs < 1 {
		secs = 1
	}
	if max := int(rate.Window.Seconds()); max > 0 && secs > max {
		secs = max
	}
	return secs
}

// --- key functions ---------------------------------------------------------

// ByIP keys on the real client IP. Correct only when trusted proxies are
// configured (see ApplyTrustedProxies); otherwise gin trusts every hop and the
// X-Forwarded-For is client-spoofable.
func ByIP(c *gin.Context) string { return "ip:" + c.ClientIP() }

// ByUserOrIP keys authenticated requests per principal and everyone else per IP
// — the right default for mixed public/authenticated routers, and resistant to
// XFF spoofing on the authenticated path.
func ByUserOrIP(c *gin.Context) string {
	if id := middleware.PrincipalIDFrom(c); id != "" {
		return "user:" + id
	}
	return "ip:" + c.ClientIP()
}

// ByHeader keys on a request header value (e.g. an API key), falling back to IP
// when the header is absent.
func ByHeader(name string) KeyFunc {
	return func(c *gin.Context) string {
		if v := c.GetHeader(name); v != "" {
			return "hdr:" + name + ":" + v
		}
		return "ip:" + c.ClientIP()
	}
}

// Composite keys on the non-empty results of several KeyFuncs joined together,
// so a request must stay under the limit for every dimension (e.g. per-IP AND
// per-account brute-force protection). Returns "" only if all sub-keys are "".
func Composite(funcs ...KeyFunc) KeyFunc {
	return func(c *gin.Context) string {
		key := ""
		for _, f := range funcs {
			if part := f(c); part != "" {
				if key != "" {
					key += "|"
				}
				key += part
			}
		}
		return key
	}
}
