-- Federation gateway: central sync + conflict resolution for federated IAG
-- deployments.
--
-- Design notes that the column definitions alone do not convey:
--   * federation_log.cursor is a BIGSERIAL, not a timestamp. Edge nodes have
--     unreliable clocks, so replication order must be assigned centrally.
--     Nodes pull "everything after cursor N" and can never miss or reorder.
--   * federation_changes exists purely for idempotency. A node that loses its
--     ack (crash, timeout) will resend; the unique (node_id, change_id) makes
--     the resend a no-op instead of a double-apply.
--   * federation_resources holds one row per federated record and is the
--     authoritative state. revision increments on every applied change and is
--     what nodes echo back as base_revision to detect divergence.

CREATE TABLE IF NOT EXISTS federation_nodes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id       TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'unknown',
    status        TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active', 'inactive', 'suspended')),
    last_seen_at  TIMESTAMPTZ,
    last_cursor   BIGINT NOT NULL DEFAULT 0,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS federation_resources (
    resource_type   TEXT   NOT NULL,
    resource_id     TEXT   NOT NULL,
    revision        BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    payload         JSONB  NOT NULL DEFAULT '{}'::jsonb,
    deleted         BOOLEAN NOT NULL DEFAULT FALSE,
    origin_node_id  TEXT   NOT NULL DEFAULT '',
    origin_stamp_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (resource_type, resource_id)
);

CREATE INDEX IF NOT EXISTS idx_federation_resources_updated
    ON federation_resources (updated_at DESC);

-- Append-only replication log. Nodes pull deltas by cursor.
CREATE TABLE IF NOT EXISTS federation_log (
    cursor         BIGSERIAL PRIMARY KEY,
    resource_type  TEXT   NOT NULL,
    resource_id    TEXT   NOT NULL,
    op             TEXT   NOT NULL CHECK (op IN ('upsert', 'delete')),
    revision       BIGINT NOT NULL,
    payload        JSONB,
    origin_node_id TEXT   NOT NULL DEFAULT '',
    applied_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_federation_log_resource
    ON federation_log (resource_type, resource_id, cursor DESC);

-- Idempotency ledger: one row per change a node has ever submitted.
CREATE TABLE IF NOT EXISTS federation_changes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id      TEXT NOT NULL,
    change_id    TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    revision     BIGINT,
    cursor       BIGINT,
    conflict_id  UUID,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (node_id, change_id)
);

CREATE TABLE IF NOT EXISTS federation_conflicts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    node_id         TEXT NOT NULL,
    change_id       TEXT NOT NULL,
    op              TEXT NOT NULL CHECK (op IN ('upsert', 'delete')),
    base_revision   BIGINT NOT NULL,
    server_revision BIGINT NOT NULL,
    node_payload    JSONB,
    server_payload  JSONB,
    state           TEXT NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'resolved')),
    resolution      TEXT CHECK (resolution IN ('keep_server', 'keep_node', 'merged')),
    resolved_by     TEXT,
    resolved_at     TIMESTAMPTZ,
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A resolved conflict must say how it was resolved, and a pending one must
    -- not pretend to be settled. Enforced here so no code path can half-resolve.
    CONSTRAINT federation_conflicts_resolution_consistent CHECK (
        (state = 'pending'  AND resolution IS NULL AND resolved_at IS NULL) OR
        (state = 'resolved' AND resolution IS NOT NULL AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_federation_conflicts_pending
    ON federation_conflicts (state, detected_at DESC);

CREATE INDEX IF NOT EXISTS idx_federation_conflicts_resource
    ON federation_conflicts (resource_type, resource_id);

-- Transactional outbox for federation.* events.
CREATE TABLE IF NOT EXISTS federation_event_outbox (
    id            BIGSERIAL PRIMARY KEY,
    event_type    TEXT NOT NULL,
    event_key     TEXT,
    payload       JSONB NOT NULL,
    attempts      INT NOT NULL DEFAULT 0,
    available_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dispatched_at TIMESTAMPTZ,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_federation_outbox_pending
    ON federation_event_outbox (available_at)
    WHERE dispatched_at IS NULL;
