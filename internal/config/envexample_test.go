package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped env examples are the first thing anyone copies. If a value in one
// of them does not actually satisfy Load(), the first run fails on a config
// error — so load them for real rather than trusting that they look right.
func TestShippedEnvExamplesLoad(t *testing.T) {
	for _, path := range []string{"../../.env.example", "../../config/.env.example"} {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			for k, v := range parseEnvFile(t, path) {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("%s does not satisfy Load(): %v", path, err)
			}
			if cfg.Audience == "" || cfg.DatabaseURL == "" {
				t.Fatalf("%s loaded but left required fields empty: %+v", path, cfg)
			}
			if !cfg.ConflictStrategy.Valid() {
				t.Fatalf("%s sets an invalid CONFLICT_STRATEGY: %q", path, cfg.ConflictStrategy)
			}
		})
	}
}

// A production-shaped environment must reject the development defaults rather
// than silently running hosted with dev-grade settings.
func TestProductionRejectsDevelopmentDefaults(t *testing.T) {
	for k, v := range parseEnvFile(t, "../../config/.env.example") {
		t.Setenv(k, v)
	}
	t.Setenv("ENVIRONMENT", "production")
	// AUTO_MIGRATE=true is the example's development default.
	if _, err := Load(); err == nil {
		t.Fatal("production with AUTO_MIGRATE=true should refuse to boot")
	}

	t.Setenv("AUTO_MIGRATE", "false")
	t.Setenv("SERVICE_CLIENT_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("production with a short SERVICE_CLIENT_SECRET should refuse to boot")
	}

	t.Setenv("SERVICE_CLIENT_SECRET", strings.Repeat("s", 32))
	if _, err := Load(); err != nil {
		t.Fatalf("production with corrected values should boot: %v", err)
	}
}

func TestInvalidConflictStrategyIsRejected(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("CONFLICT_STRATEGY", "whatever")
	if _, err := Load(); err == nil {
		t.Fatal("an unrecognised CONFLICT_STRATEGY must fail at boot, not silently change merge behaviour")
	}
}

// parseEnvFile reads KEY=VALUE lines, skipping comments and blanks.
func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// These files are edited on Windows and may carry a UTF-8 BOM, which
		// would otherwise become part of the first key name.
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s contained no variables", path)
	}
	return out
}
