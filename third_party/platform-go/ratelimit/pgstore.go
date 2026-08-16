package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of a pgx pool/conn the Postgres store needs, so callers
// can pass a *pgxpool.Pool directly.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PostgresStore is a distributed, fixed-window rate-limit Store backed by a
// shared Postgres table. Unlike the in-memory store it enforces ONE global limit
// across horizontally-scaled replicas (they share the counter row) and survives
// restarts. It trades a DB round-trip per request for that correctness, so it
// suits moderate-traffic internal services; keep the in-memory store for
// single-instance or very hot paths.
//
// Chosen over a Redis store deliberately: platform-go already depends on pgx and
// every service already runs Postgres, so this adds no new dependency anywhere.
// The Store interface is unchanged, so a Redis implementation can be dropped in
// later without touching call sites.
type PostgresStore struct {
	db    Querier
	table string
}

// PostgresStoreOption configures a PostgresStore.
type PostgresStoreOption func(*PostgresStore)

// WithTable overrides the counter table name (default "ratelimit_counters").
// The name is validated as a bare identifier since it is interpolated into DDL.
func WithTable(name string) PostgresStoreOption {
	return func(s *PostgresStore) {
		if isSafeIdentifier(name) {
			s.table = name
		}
	}
}

// NewPostgresStore returns a Postgres-backed store. Call EnsureSchema once at
// startup (or run the equivalent migration) to create the counter table.
func NewPostgresStore(db Querier, opts ...PostgresStoreOption) *PostgresStore {
	s := &PostgresStore{db: db, table: "ratelimit_counters"}
	for _, o := range opts {
		o(s)
	}
	return s
}

// EnsureSchema idempotently creates the counter table. platform-go is a library
// and cannot run migrations, so services call this at boot (or ship the DDL as a
// migration). Safe to call repeatedly.
func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	_, err := s.db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			bucket       TEXT   NOT NULL,
			window_start BIGINT NOT NULL,
			count        INT    NOT NULL DEFAULT 0,
			PRIMARY KEY (bucket, window_start)
		)`, s.table))
	return err
}

// Allow implements Store with an atomic fixed-window counter. The window is
// derived from the DATABASE clock (not the app's), so replicas with skewed
// clocks still agree on the window boundary.
func (s *PostgresStore) Allow(ctx context.Context, key string, rate Rate) (bool, time.Duration, error) {
	if !rate.Valid() {
		return true, 0, nil
	}
	windowSec := int64(rate.Window / time.Second)
	if windowSec < 1 {
		windowSec = 1
	}
	// Atomic upsert: increment the current window's counter and return the new
	// count plus seconds until the window resets. window_start is computed in SQL
	// from the DB clock so all callers bucket identically.
	var count int
	var retrySecs int64
	err := s.db.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s (bucket, window_start, count)
		VALUES ($1, (floor(extract(epoch from now()) / $2) * $2)::bigint, 1)
		ON CONFLICT (bucket, window_start)
		DO UPDATE SET count = %s.count + 1
		RETURNING count, (window_start + $2 - floor(extract(epoch from now()))::bigint)
	`, s.table, s.table), key, windowSec).Scan(&count, &retrySecs)
	if err != nil {
		return false, 0, err
	}
	if count <= rate.Requests {
		return true, 0, nil
	}
	if retrySecs < 1 {
		retrySecs = 1
	}
	return false, time.Duration(retrySecs) * time.Second, nil
}

// Cleanup deletes counter rows for windows that ended more than retain ago. Old
// windows are never read again, so this just bounds table size; call it
// periodically (e.g. a boot-time ticker). Returns the number of rows removed.
func (s *PostgresStore) Cleanup(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		retain = time.Hour
	}
	cutoff := time.Now().Add(-retain).Unix()
	tag, err := s.db.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE window_start < $1`, s.table), cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// isSafeIdentifier permits only a bare unquoted SQL identifier (letters, digits,
// underscore; not starting with a digit) so a custom table name can't inject SQL
// into the DDL/DML above.
func isSafeIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
