package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresDedupe returns a Dedupe backed by a per-service processed_events
// table. The caller is expected to have already created the table:
//
//	CREATE TABLE IF NOT EXISTS <schema>.processed_events (
//	    event_id TEXT PRIMARY KEY,
//	    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
//	);
//
// table must be the fully qualified name (e.g. "notifications.processed_events").
func PostgresDedupe(pool *pgxpool.Pool, table string) Dedupe {
	if pool == nil {
		panic("events: PostgresDedupe requires a non-nil pool")
	}
	if table == "" {
		panic("events: PostgresDedupe requires a qualified table name")
	}
	return &pgDedupe{pool: pool, table: table}
}

type pgDedupe struct {
	pool  *pgxpool.Pool
	table string
}

func (d *pgDedupe) Seen(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	var found int
	err := d.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT 1 FROM %s WHERE event_id = $1`, d.table), eventID,
	).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (d *pgDedupe) Mark(ctx context.Context, eventID string) error {
	if eventID == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, d.table),
		eventID,
	)
	return err
}
