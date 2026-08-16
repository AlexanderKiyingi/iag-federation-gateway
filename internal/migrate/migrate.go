// Package migrate applies embedded SQL migrations idempotently, tracking
// applied versions and their checksums in schema_migrations.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

type Migration struct {
	Version  string
	Body     string
	Checksum string
}

func Up(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	if _, err := pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	migs, err := load(fsys)
	if err != nil {
		return nil, err
	}
	applied, err := loadApplied(ctx, pool)
	if err != nil {
		return nil, err
	}
	var newlyApplied []string
	for _, m := range migs {
		prev, ok := applied[m.Version]
		switch {
		case !ok:
			if err := apply(ctx, pool, m); err != nil {
				return newlyApplied, fmt.Errorf("migration %s: %w", m.Version, err)
			}
			newlyApplied = append(newlyApplied, m.Version)
			slog.Info("migration applied", "version", m.Version)
		case prev.Checksum != m.Checksum:
			return newlyApplied, fmt.Errorf("migration %s checksum mismatch", m.Version)
		}
	}
	return newlyApplied, nil
}

func load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  strings.TrimSuffix(name, ".sql"),
			Body:     string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

type appliedRow struct {
	Version  string
	Checksum string
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[string]appliedRow, error) {
	rows, err := pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]appliedRow{}
	for rows.Next() {
		var r appliedRow
		if err := rows.Scan(&r.Version, &r.Checksum); err != nil {
			return nil, err
		}
		out[r.Version] = r
	}
	return out, rows.Err()
}

func apply(ctx context.Context, pool *pgxpool.Pool, m Migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, m.Body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
		m.Version, m.Checksum); err != nil {
		if strings.Contains(err.Error(), "23505") {
			return errors.New("concurrent migration")
		}
		return err
	}
	return tx.Commit(ctx)
}
