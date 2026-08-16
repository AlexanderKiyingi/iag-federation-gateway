package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresStore runs against a real Postgres when TEST_DATABASE_URL is set
// (e.g. a throwaway cluster); otherwise it is skipped so the default `go test`
// stays hermetic.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the Postgres store test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool, WithTable("ratelimit_counters_test"))
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS ratelimit_counters_test"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// EnsureSchema is idempotent.
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema (2nd): %v", err)
	}

	rate := Rate{Requests: 3, Window: time.Minute}
	key := "ip:198.51.100.9"
	for i := 0; i < 3; i++ {
		ok, _, err := s.Allow(ctx, key, rate)
		if err != nil || !ok {
			t.Fatalf("request %d should be allowed (ok=%v err=%v)", i, ok, err)
		}
	}
	ok, retry, err := s.Allow(ctx, key, rate)
	if err != nil {
		t.Fatalf("4th allow err: %v", err)
	}
	if ok {
		t.Fatal("4th request in the window should be throttled")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry-after should be within the window, got %v", retry)
	}

	// A different key is independent.
	if ok, _, _ := s.Allow(ctx, "ip:203.0.113.1", rate); !ok {
		t.Fatal("a different key must have its own budget")
	}

	// Cleanup removes nothing recent (current window is not stale).
	if n, err := s.Cleanup(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("cleanup should remove no recent rows (n=%d err=%v)", n, err)
	}
}
