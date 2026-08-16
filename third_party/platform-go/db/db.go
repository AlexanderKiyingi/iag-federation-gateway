// Package db owns Postgres connection pooling for IAG services.
//
// It exists because pool configuration was previously hand-rolled per service
// and drifted badly: some services capped connections and kept a warm minimum,
// others ran on driver defaults with no minimum and paid a fresh connect on
// every traffic burst. On a platform where ~23 services share one Postgres
// instance, that difference is the gap between "slow" and "too many clients".
//
// Typical wiring in a service:
//
//	pool, err := db.Connect(ctx, db.ConfigFromEnv("iag_fleet, public"))
//	if err != nil { return fmt.Errorf("connect database: %w", err) }
//	defer pool.Close()
//
// # Connection poolers
//
// Set DB_POOLER_MODE=transaction when the service connects through PgBouncer in
// transaction pooling mode. This is not cosmetic — transaction pooling hands a
// different server connection to each transaction, which breaks two things that
// otherwise look fine in development:
//
//   - Persistent prepared statements. pgx caches them per connection by default;
//     under transaction pooling the cached statement may not exist on the server
//     connection that receives the next query. Transaction mode therefore
//     defaults to QueryExecModeExec, which uses the extended protocol without
//     leaving named statements behind.
//
//   - Session state set with SET. A service that pins its schema via
//     `AfterConnect: SET search_path` silently loses it, because the SET applies
//     to one server connection and the next transaction may land on another.
//     This package always sets search_path as a startup RuntimeParam instead,
//     which PgBouncer tracks and applies per server connection. Do not
//     reintroduce an AfterConnect SET for session state.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecMode selects the pgx query execution mode.
//
// This mirrors pgx's own enum rather than reusing it because pgx's zero value
// is QueryExecModeCacheStatement — a real, meaningful mode. Reusing it would
// make "caller did not set this" indistinguishable from "caller deliberately
// chose statement caching", and silently override the latter with the
// pooler default.
type ExecMode int

const (
	// ExecModeDefault defers to the pooler-appropriate default.
	ExecModeDefault ExecMode = iota
	ExecModeCacheStatement
	ExecModeCacheDescribe
	ExecModeDescribeExec
	ExecModeExec
	ExecModeSimpleProtocol
)

// pgxMode maps to the driver enum. Reports false for ExecModeDefault and for
// any value outside the known set.
func (m ExecMode) pgxMode() (pgx.QueryExecMode, bool) {
	switch m {
	case ExecModeCacheStatement:
		return pgx.QueryExecModeCacheStatement, true
	case ExecModeCacheDescribe:
		return pgx.QueryExecModeCacheDescribe, true
	case ExecModeDescribeExec:
		return pgx.QueryExecModeDescribeExec, true
	case ExecModeExec:
		return pgx.QueryExecModeExec, true
	case ExecModeSimpleProtocol:
		return pgx.QueryExecModeSimpleProtocol, true
	}
	return 0, false
}

// PoolerMode describes what sits between the service and Postgres.
type PoolerMode string

const (
	// PoolerSession is a direct connection to Postgres, or a pooler in session
	// mode. Prepared statements and session state behave normally.
	PoolerSession PoolerMode = "session"
	// PoolerTransaction is PgBouncer (or equivalent) in transaction pooling
	// mode. Server connections are shared per transaction.
	PoolerTransaction PoolerMode = "transaction"
)

// Config configures the pool. Zero-valued fields take the defaults documented
// on each field, which differ by PoolerMode where that materially matters.
type Config struct {
	// URL is the Postgres DSN. Defaults to $DATABASE_URL.
	URL string
	// SearchPath pins the service's schema, e.g. "iag_fleet, public". Applied as
	// a startup parameter so it survives transaction pooling. Empty leaves the
	// server default. Always include public as a fallback for shared extensions
	// and any not-yet-relocated tables.
	SearchPath string
	// Pooler selects pooler-safe behaviour. Defaults to PoolerSession.
	Pooler PoolerMode
	// MaxConns caps connections held by this service instance. Defaults to 10
	// direct (23 services on one instance add up fast) or 25 behind a
	// transaction pooler, which multiplexes them onto far fewer server
	// connections. Remember the real figure is MaxConns × replica count.
	MaxConns int32
	// MinConns is the warm floor. Defaults to 2 — the point is that a burst of
	// traffic does not pay TCP and TLS setup before its first query.
	MinConns int32
	// MaxConnLifetime recycles connections so a long-lived pool cannot pin a
	// stale server-side state or defeat a failover. Defaults to 1 hour.
	MaxConnLifetime time.Duration
	// MaxConnLifetimeJitter spreads recycling so a pool does not reconnect in
	// lockstep after a deploy. Defaults to 5 minutes.
	MaxConnLifetimeJitter time.Duration
	// MaxConnIdleTime releases idle connections back. Defaults to 15 minutes.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod controls background liveness checks. Defaults to 1 min.
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds a single connection attempt. Defaults to 10s.
	ConnectTimeout time.Duration
	// ExecMode overrides the query execution mode. Leave as ExecModeDefault to
	// take the pooler-appropriate choice (see PoolerMode). Override only with a
	// specific reason — simple protocol is the most permissive and the least
	// efficient, and it gives up real parameter binding.
	ExecMode ExecMode
	// PingTimeout bounds the startup connectivity check. Defaults to 5s.
	PingTimeout time.Duration
}

// ConfigFromEnv builds a Config from the standard IAG database variables,
// taking searchPath from the caller because it is a property of the service,
// not of the environment it runs in.
//
//	DATABASE_URL                 required
//	DB_POOLER_MODE               session (default) | transaction
//	DB_MAX_CONNS                 int
//	DB_MIN_CONNS                 int
//	DB_MAX_CONN_LIFETIME         duration, e.g. 1h
//	DB_MAX_CONN_IDLE_TIME        duration, e.g. 15m
//	DB_CONNECT_TIMEOUT           duration, e.g. 10s
//	DB_QUERY_EXEC_MODE           cache_statement | cache_describe | describe_exec | exec | simple
//
// Unparseable values are ignored in favour of the default rather than failing
// the process: a malformed tuning value should not stop a service booting.
func ConfigFromEnv(searchPath string) Config {
	cfg := Config{
		URL:        os.Getenv("DATABASE_URL"),
		SearchPath: searchPath,
		Pooler:     PoolerSession,
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DB_POOLER_MODE")), string(PoolerTransaction)) {
		cfg.Pooler = PoolerTransaction
	}
	if v, ok := intEnv("DB_MAX_CONNS"); ok {
		cfg.MaxConns = int32(v)
	}
	if v, ok := intEnv("DB_MIN_CONNS"); ok {
		cfg.MinConns = int32(v)
	}
	cfg.MaxConnLifetime, _ = durationEnv("DB_MAX_CONN_LIFETIME")
	cfg.MaxConnIdleTime, _ = durationEnv("DB_MAX_CONN_IDLE_TIME")
	cfg.ConnectTimeout, _ = durationEnv("DB_CONNECT_TIMEOUT")
	if m, ok := parseExecMode(os.Getenv("DB_QUERY_EXEC_MODE")); ok {
		cfg.ExecMode = m
	}
	return cfg
}

// Connect builds the pool and verifies it with a bounded ping, so a bad DSN or
// an unreachable database fails at boot with a clear error rather than on the
// first request.
func Connect(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := BuildPoolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, orDuration(cfg.PingTimeout, 5*time.Second))
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// BuildPoolConfig applies defaults and pooler-safe settings to a parsed DSN.
// Exported so services can make one-off adjustments, and so the pooler
// behaviour is testable without a live database.
func BuildPoolConfig(cfg Config) (*pgxpool.Config, error) {
	url := cfg.URL
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("db: DATABASE_URL is empty")
	}

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}

	transaction := cfg.Pooler == PoolerTransaction

	poolCfg.MaxConns = orInt32(cfg.MaxConns, defaultMaxConns(transaction))
	poolCfg.MinConns = orInt32(cfg.MinConns, 2)
	poolCfg.MaxConnLifetime = orDuration(cfg.MaxConnLifetime, time.Hour)
	poolCfg.MaxConnLifetimeJitter = orDuration(cfg.MaxConnLifetimeJitter, 5*time.Minute)
	poolCfg.MaxConnIdleTime = orDuration(cfg.MaxConnIdleTime, 15*time.Minute)
	poolCfg.HealthCheckPeriod = orDuration(cfg.HealthCheckPeriod, time.Minute)
	poolCfg.ConnConfig.ConnectTimeout = orDuration(cfg.ConnectTimeout, 10*time.Second)

	// MinConns above MaxConns would have the pool fighting itself; clamp rather
	// than refuse to boot over a tuning mistake.
	if poolCfg.MinConns > poolCfg.MaxConns {
		poolCfg.MinConns = poolCfg.MaxConns
	}

	// Pin the schema as a startup parameter, overriding whatever the DSN carried.
	// Startup params survive transaction pooling; an AfterConnect `SET` does not.
	if sp := strings.TrimSpace(cfg.SearchPath); sp != "" {
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["search_path"] = sp
	}

	mode, ok := cfg.ExecMode.pgxMode()
	if !ok {
		mode = defaultQueryExecMode(transaction)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = mode

	// Named prepared statements outlive a transaction-pooled server connection,
	// so the caches must be off rather than merely unused.
	if transaction {
		poolCfg.ConnConfig.StatementCacheCapacity = 0
		poolCfg.ConnConfig.DescriptionCacheCapacity = 0
	}

	return poolCfg, nil
}

// defaultMaxConns keeps a direct pool deliberately small: every service holds
// its own, and the shared instance sees the sum. Behind a transaction pooler
// the client-side figure is decoupled from server connections, so it can be
// larger without the same risk.
func defaultMaxConns(transaction bool) int32 {
	if transaction {
		return 25
	}
	return 10
}

// defaultQueryExecMode picks the safest mode that still uses the extended
// protocol. QueryExecModeExec sends parse and bind together without leaving a
// named statement on the server, which is what makes it transaction-pooler
// safe while keeping proper parameter binding — unlike simple protocol, which
// interpolates values into the query text.
func defaultQueryExecMode(transaction bool) pgx.QueryExecMode {
	if transaction {
		return pgx.QueryExecModeExec
	}
	return pgx.QueryExecModeCacheStatement
}

func parseExecMode(s string) (ExecMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cache_statement":
		return ExecModeCacheStatement, true
	case "cache_describe":
		return ExecModeCacheDescribe, true
	case "describe_exec":
		return ExecModeDescribeExec, true
	case "exec":
		return ExecModeExec, true
	case "simple":
		return ExecModeSimpleProtocol, true
	}
	return ExecModeDefault, false
}

func intEnv(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func durationEnv(key string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func orInt32(v, fallback int32) int32 {
	if v > 0 {
		return v
	}
	return fallback
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}
