package db

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testDSN = "postgres://user:pass@localhost:5432/iag?sslmode=disable"

func TestBuildPoolConfigDefaults(t *testing.T) {
	cfg, err := BuildPoolConfig(Config{URL: testDSN})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if cfg.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want 10 (direct pools stay small; 23 services share one instance)", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Errorf("MinConns = %d, want 2 — a warm floor is the point of this package", cfg.MinConns)
	}
	if cfg.MaxConnLifetime != time.Hour {
		t.Errorf("MaxConnLifetime = %v, want 1h", cfg.MaxConnLifetime)
	}
	if cfg.MaxConnLifetimeJitter != 5*time.Minute {
		t.Errorf("MaxConnLifetimeJitter = %v, want 5m", cfg.MaxConnLifetimeJitter)
	}
	if cfg.ConnConfig.ConnectTimeout != 10*time.Second {
		t.Errorf("ConnectTimeout = %v, want 10s", cfg.ConnConfig.ConnectTimeout)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Errorf("exec mode = %v, want CacheStatement for a direct connection", cfg.ConnConfig.DefaultQueryExecMode)
	}
}

// The whole reason this package exists: transaction pooling must not leave
// named prepared statements behind, because the next transaction may land on a
// different server connection.
func TestTransactionPoolerIsPreparedStatementSafe(t *testing.T) {
	cfg, err := BuildPoolConfig(Config{URL: testDSN, Pooler: PoolerTransaction})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Errorf("exec mode = %v, want Exec under transaction pooling", cfg.ConnConfig.DefaultQueryExecMode)
	}
	if cfg.ConnConfig.StatementCacheCapacity != 0 {
		t.Errorf("StatementCacheCapacity = %d, want 0 under transaction pooling", cfg.ConnConfig.StatementCacheCapacity)
	}
	if cfg.ConnConfig.DescriptionCacheCapacity != 0 {
		t.Errorf("DescriptionCacheCapacity = %d, want 0 under transaction pooling", cfg.ConnConfig.DescriptionCacheCapacity)
	}
	if cfg.MaxConns != 25 {
		t.Errorf("MaxConns = %d, want 25 behind a pooler", cfg.MaxConns)
	}
}

// search_path must be a startup parameter, not an AfterConnect SET, or it is
// silently lost the moment the service moves behind a transaction pooler.
func TestSearchPathIsAStartupParameter(t *testing.T) {
	cfg, err := BuildPoolConfig(Config{URL: testDSN, SearchPath: "iag_fleet, public"})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != "iag_fleet, public" {
		t.Errorf("search_path runtime param = %q, want %q", got, "iag_fleet, public")
	}
	if cfg.AfterConnect != nil {
		t.Error("AfterConnect must stay nil — session state set there does not survive transaction pooling")
	}
}

// A DSN carrying its own search_path must not win over the service's explicit
// one, or relocating a service's schema becomes an environment-variable hunt.
func TestSearchPathOverridesTheDSN(t *testing.T) {
	cfg, err := BuildPoolConfig(Config{
		URL:        testDSN + "&search_path=public",
		SearchPath: "iag_finance, public",
	})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != "iag_finance, public" {
		t.Errorf("search_path = %q, want the explicit config value to win", got)
	}
}

func TestExplicitExecModeSurvivesTheDefault(t *testing.T) {
	// CacheStatement is pgx's zero value, so this is the case that regresses if
	// anyone reintroduces an `if mode == 0` check.
	cfg, err := BuildPoolConfig(Config{
		URL:      testDSN,
		Pooler:   PoolerTransaction,
		ExecMode: ExecModeCacheStatement,
	})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Errorf("exec mode = %v, want the caller's explicit CacheStatement to be honoured", cfg.ConnConfig.DefaultQueryExecMode)
	}
}

func TestMinConnsClampedToMax(t *testing.T) {
	cfg, err := BuildPoolConfig(Config{URL: testDSN, MaxConns: 4, MinConns: 30})
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if cfg.MinConns != 4 {
		t.Errorf("MinConns = %d, want clamping to MaxConns rather than a boot failure", cfg.MinConns)
	}
}

func TestEmptyURLIsAnError(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := BuildPoolConfig(Config{}); err == nil {
		t.Fatal("want an error for an empty DSN")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("DB_POOLER_MODE", "transaction")
	t.Setenv("DB_MAX_CONNS", "40")
	t.Setenv("DB_MIN_CONNS", "5")
	t.Setenv("DB_MAX_CONN_LIFETIME", "30m")
	t.Setenv("DB_QUERY_EXEC_MODE", "simple")

	cfg := ConfigFromEnv("iag_warehouse, public")
	if cfg.Pooler != PoolerTransaction {
		t.Errorf("Pooler = %q, want transaction", cfg.Pooler)
	}
	if cfg.MaxConns != 40 || cfg.MinConns != 5 {
		t.Errorf("conns = %d/%d, want 40/5", cfg.MaxConns, cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %v, want 30m", cfg.MaxConnLifetime)
	}
	if cfg.ExecMode != ExecModeSimpleProtocol {
		t.Errorf("ExecMode = %v, want simple", cfg.ExecMode)
	}
	if cfg.SearchPath != "iag_warehouse, public" {
		t.Errorf("SearchPath = %q", cfg.SearchPath)
	}
}

// A typo in a tuning variable should cost the default, not the process.
func TestMalformedEnvFallsBackToDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("DB_MAX_CONNS", "lots")
	t.Setenv("DB_MAX_CONN_LIFETIME", "one hour")
	t.Setenv("DB_QUERY_EXEC_MODE", "turbo")

	cfg := ConfigFromEnv("public")
	poolCfg, err := BuildPoolConfig(cfg)
	if err != nil {
		t.Fatalf("BuildPoolConfig: %v", err)
	}
	if poolCfg.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want the default 10", poolCfg.MaxConns)
	}
	if poolCfg.MaxConnLifetime != time.Hour {
		t.Errorf("MaxConnLifetime = %v, want the default 1h", poolCfg.MaxConnLifetime)
	}
	if poolCfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Errorf("exec mode = %v, want the default", poolCfg.ConnConfig.DefaultQueryExecMode)
	}
}
