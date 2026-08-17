// Package runtimeenv decides whether a service should apply production
// safeguards — chiefly fail-closed RBAC, where a verified token carrying an
// empty permissions array is denied rather than granted everything.
//
// It exists because that decision used to be `Environment == "production"`, and
// the Railway runbooks never told anyone to set ENVIRONMENT. Every hosted
// service therefore fell back to the "development" default and ran fail-OPEN.
// The fix was written thirteen times, once per service, and drifted: twelve
// copies read the process environment at call time, one captured it at load
// time, and two services under edge/ never got it at all because the rollout
// walked services/** and edge/ is a peer of that, not a child.
//
// One implementation removes the drift and, more usefully, removes the need to
// find every copy next time.
//
// The rule: an explicitly configured environment is obeyed — dev-like values
// opt out, anything else hardens. With nothing configured, a process that looks
// deployed hardens and a laptop does not. Getting this wrong can only ever cost
// a 403 for a caller that should not have had access; it cannot prevent boot.
package runtimeenv

import (
	"os"
	"strings"
)

// Hardened reports whether production safeguards apply to a service configured
// with the given environment value, reading deployment signals from the
// process. This is the form most services want:
//
//	func (c Config) HardenedRuntime() bool { return runtimeenv.Hardened(c.Environment) }
func Hardened(environment string) bool {
	return HardenedFrom(environment, ExplicitlySet(), Deployed())
}

// HardenedFrom is Hardened with the process signals supplied by the caller. It
// reads nothing and is therefore trivially testable, which is what a service
// that captures these on its Config at load time should use:
//
//	func (c Config) HardenedRuntime() bool {
//	    return runtimeenv.HardenedFrom(c.Environment, c.EnvironmentExplicit, c.Deployed)
//	}
//
// Capturing at load time is the better shape — it keeps Config a plain value
// with no hidden dependency on the process — so both are supported rather than
// forcing every service onto the env-reading variant.
func HardenedFrom(environment string, explicitlySet, deployed bool) bool {
	// An explicit production value always hardens, including on a Config built
	// by hand in a test rather than through Load. Anything else would make the
	// safe setting depend on a bookkeeping flag the caller did not know to set.
	if IsProduction(environment) {
		return true
	}
	if explicitlySet {
		return !IsDevLike(environment)
	}
	return deployed
}

// IsProduction reports whether the value names production.
func IsProduction(environment string) bool {
	switch normalize(environment) {
	case "production", "prod":
		return true
	}
	return false
}

// IsDevLike reports an environment where fail-open behaviour is a deliberate
// local convenience rather than an accident.
func IsDevLike(environment string) bool {
	switch normalize(environment) {
	case "development", "dev", "local", "test":
		return true
	}
	return false
}

// ExplicitlySet distinguishes a deliberately configured environment from the
// "development" value most Load functions fall back to when nothing is set.
//
// An unrecognised value counts as explicit and therefore hardens: "staging" is
// not on the dev-like list, and a deployment nobody anticipated should get the
// safe behaviour rather than the convenient one.
func ExplicitlySet() bool {
	return getenv("ENVIRONMENT") != "" || getenv("APP_ENV") != ""
}

// Deployed distinguishes a hosted instance from a laptop: Railway's injected
// variables, or gin in release mode, which the service Dockerfiles set.
//
// It is deliberately a guess biased towards hardening. A laptop that happens to
// set GIN_MODE=release gets fail-closed RBAC, which is an inconvenience; a
// production host mistaken for a laptop gets fail-open, which is a breach.
func Deployed() bool {
	if getenv("RAILWAY_ENVIRONMENT") != "" || getenv("RAILWAY_PROJECT_ID") != "" {
		return true
	}
	return strings.EqualFold(getenv("GIN_MODE"), "release")
}

func getenv(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
