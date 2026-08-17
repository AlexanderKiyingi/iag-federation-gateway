// Package config loads the federation-gateway runtime configuration from the
// environment. The gateway is a central service: it accepts platform Bearer
// tokens (aud=iag.federation-gateway) from both operators and edge-node service
// accounts, and mints its own service token only to register its permission
// catalogue with iag-authentication.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alvor-technologies/iag-platform-go/corsenv"
	"github.com/iag/federation-gateway/internal/models"
)

type Config struct {
	ServiceName string
	Addr        string
	Environment string
	DatabaseURL string

	// Inbound auth (verified on every /v1 request).
	JWTIssuer string
	JWKSURL   string
	Audience  string

	// Outbound service-account auth (permission-catalogue registration).
	ServiceClientID     string
	ServiceClientSecret string
	AuthTokenURL        string

	CORSOrigin  string
	AutoMigrate bool

	// ConflictStrategy is the default automatic resolution policy.
	ConflictStrategy models.Strategy

	// MaxPushBatch caps how many changes one push may carry. A push is applied
	// in a single transaction, so an unbounded batch would hold row locks for
	// as long as it takes to process — starving other nodes.
	MaxPushBatch int
	// MaxPullBatch caps a delta pull.
	MaxPullBatch int

	EventBusEnabled bool
	KafkaBrokers    []string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func Load() (Config, error) {
	env := strings.ToLower(strings.TrimSpace(envOr("ENVIRONMENT", envOr("APP_ENV", "development"))))
	issuer := envOr("JWT_ISSUER", "http://localhost:3001")

	cfg := Config{
		ServiceName: envOr("SERVICE_NAME", "federation-gateway"),
		Addr:        ListenAddr(),
		Environment: env,
		DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),

		JWTIssuer: issuer,
		JWKSURL:   envOr("JWKS_URL", strings.TrimRight(issuer, "/")+"/.well-known/jwks.json"),
		Audience:  envOr("AUDIENCE", "iag.federation-gateway"),

		ServiceClientID:     envOr("SERVICE_CLIENT_ID", "iag-federation-gateway"),
		ServiceClientSecret: strings.TrimSpace(os.Getenv("SERVICE_CLIENT_SECRET")),
		AuthTokenURL:        envOr("AUTH_TOKEN_URL", strings.TrimRight(issuer, "/")+"/oauth/token"),

		CORSOrigin:  corsenv.Allowlist(corsenv.DefaultDevOrigins),
		AutoMigrate: envOr("AUTO_MIGRATE", "true") != "false",

		ConflictStrategy: models.Strategy(strings.ToLower(strings.TrimSpace(
			envOr("CONFLICT_STRATEGY", string(models.StrategyLastWriteWins))))),

		MaxPushBatch: intOr("MAX_PUSH_BATCH", 200),
		MaxPullBatch: intOr("MAX_PULL_BATCH", 500),

		EventBusEnabled: strings.EqualFold(os.Getenv("EVENT_BUS_ENABLED"), "true"),
		KafkaBrokers:    parseList(os.Getenv("KAFKA_BROKERS")),

		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.Audience == "" {
		return fmt.Errorf("AUDIENCE is required (e.g. iag.federation-gateway)")
	}
	if c.JWKSURL == "" {
		return fmt.Errorf("JWKS_URL is required")
	}
	// A bad strategy string must fail at boot, not silently degrade to a
	// different merge policy — that would corrupt data quietly.
	if !c.ConflictStrategy.Valid() {
		return fmt.Errorf("CONFLICT_STRATEGY %q is invalid (last_write_wins|server_wins|node_wins|manual)", c.ConflictStrategy)
	}
	if c.MaxPushBatch <= 0 {
		return fmt.Errorf("MAX_PUSH_BATCH must be positive")
	}
	if c.MaxPullBatch <= 0 {
		return fmt.Errorf("MAX_PULL_BATCH must be positive")
	}
	if c.IsProduction() {
		if c.HasWildcardCORS() {
			return fmt.Errorf("set ALLOWED_ORIGINS in production (not *)")
		}
		if len(strings.TrimSpace(c.ServiceClientSecret)) < 16 {
			return fmt.Errorf("SERVICE_CLIENT_SECRET must be at least 16 characters in production")
		}
		if c.AutoMigrate {
			return fmt.Errorf("AUTO_MIGRATE must be false in production (run migrations out of band)")
		}
	}
	return nil
}

func (c Config) IsProduction() bool {
	return c.Environment == "production" || c.Environment == "prod"
}

// StrictRBAC denies access when a verified token carries no permissions
// (fail-closed).
func (c Config) StrictRBAC() bool { return c.HardenedRuntime() }

// HardenedRuntime reports whether production safeguards apply.
//
// It deliberately does not just return IsProduction(). That required
// ENVIRONMENT=production, which the Railway runbooks never told anyone to set,
// so a hosted instance fell back to the "development" default and ran
// fail-OPEN: auth.RequirePermission grants EVERY permission to a token carrying
// an empty permissions array. An unset ENVIRONMENT on a deployed instance now
// hardens instead; only an explicit dev-like value opts out.
//
// This cannot prevent boot — the worst case is a 403 for a caller that should
// never have had access. Boot-time validation stays keyed on ENVIRONMENT alone.
//
// Mirrors the implementation shipped across the domain services; the intent is
// one shared implementation in shared/platform-go once every service is on it.
func (c Config) HardenedRuntime() bool {
	// An explicit production value always hardens, including on a Config built
	// by hand in a test rather than through Load.
	if c.IsProduction() {
		return true
	}
	if environmentExplicitlySet() {
		return !c.isDevLike()
	}
	return deployedRuntime()
}

// isDevLike reports an environment where fail-open behaviour is a deliberate
// local convenience rather than an accident.
func (c Config) isDevLike() bool {
	switch c.Environment {
	case "development", "dev", "local", "test":
		return true
	}
	return false
}

// environmentExplicitlySet distinguishes a deliberately configured environment
// from the "development" value Load falls back to when nothing is set. Read
// from the process rather than captured on Config: StrictRBAC is resolved once
// during startup wiring, and the environment does not change under us.
func environmentExplicitlySet() bool {
	return strings.TrimSpace(os.Getenv("ENVIRONMENT")) != "" ||
		strings.TrimSpace(os.Getenv("APP_ENV")) != ""
}

// deployedRuntime distinguishes a hosted instance from a laptop: Railway's
// injected variables, or gin in release mode, which the Dockerfiles set.
func deployedRuntime() bool {
	if strings.TrimSpace(os.Getenv("RAILWAY_ENVIRONMENT")) != "" ||
		strings.TrimSpace(os.Getenv("RAILWAY_PROJECT_ID")) != "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GIN_MODE")), "release")
}

func (c Config) HasWildcardCORS() bool {
	for _, o := range strings.Split(c.CORSOrigin, ",") {
		if strings.TrimSpace(o) == "*" {
			return true
		}
	}
	return c.CORSOrigin == "*"
}

func parseList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intOr(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
