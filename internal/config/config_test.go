package config

import (
	"strings"
	"testing"

	"github.com/iag/federation-gateway/internal/models"
)

func baseConfig() Config {
	return Config{
		DatabaseURL:      "postgres://localhost/db",
		Audience:         "iag.federation-gateway",
		JWKSURL:          "http://localhost:3001/.well-known/jwks.json",
		ConflictStrategy: models.StrategyLastWriteWins,
		MaxPushBatch:     200,
		MaxPullBatch:     500,
		CORSOrigin:       "https://example.com",
	}
}

func TestValidate_acceptsSaneConfig(t *testing.T) {
	if err := baseConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// A typo in CONFLICT_STRATEGY must stop the boot. Silently falling back to a
// different merge policy would corrupt data in a way nobody would notice until
// records had already diverged.
func TestValidate_rejectsUnknownStrategy(t *testing.T) {
	c := baseConfig()
	c.ConflictStrategy = models.Strategy("lastwritewins")
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an invalid strategy to be rejected")
	}
	if !strings.Contains(err.Error(), "CONFLICT_STRATEGY") {
		t.Errorf("error should name the offending variable, got: %v", err)
	}
}

func TestValidate_requiresDatabaseURL(t *testing.T) {
	c := baseConfig()
	c.DatabaseURL = ""
	if err := c.Validate(); err == nil {
		t.Fatal("expected missing DATABASE_URL to be rejected")
	}
}

func TestValidate_productionGuards(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*Config)
		want  string
	}{
		{"wildcard cors", func(c *Config) { c.CORSOrigin = "*" }, "ALLOWED_ORIGINS"},
		{"short secret", func(c *Config) { c.ServiceClientSecret = "tooshort" }, "SERVICE_CLIENT_SECRET"},
		{"automigrate on", func(c *Config) { c.AutoMigrate = true }, "AUTO_MIGRATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.Environment = "production"
			c.ServiceClientSecret = "a-sufficiently-long-secret"
			c.AutoMigrate = false
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected production guard to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestValidate_devAllowsLooseSettings(t *testing.T) {
	c := baseConfig()
	c.Environment = "development"
	c.CORSOrigin = "*"
	c.AutoMigrate = true
	c.ServiceClientSecret = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("dev config rejected: %v", err)
	}
}

func TestStrictRBACOnlyInProduction(t *testing.T) {
	c := baseConfig()
	c.Environment = "development"
	if c.StrictRBAC() {
		t.Error("dev should not enforce strict RBAC")
	}
	c.Environment = "production"
	if !c.StrictRBAC() {
		t.Error("production must enforce strict RBAC")
	}
}

func TestListenAddrNormalisation(t *testing.T) {
	t.Setenv("PORT", "4021")
	if got := ListenAddr(); got != ":4021" {
		t.Fatalf("ListenAddr() = %q, want :4021", got)
	}
}
