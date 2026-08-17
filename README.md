# iag-federation-gateway

Central **sync and conflict resolution** for federated IAG deployments.

Edge deployments (depot nodes, POS terminals, field apps) keep working without
connectivity: they capture and mutate records locally, then push them here when
the link returns. Because two nodes can edit the same record during the same
offline window, sync is not a copy — it is a **merge with an explicit conflict
policy**. That arbitration is what this service exists to do.

Headless Go/Gin JSON API — no bundled UI.

## How it works

```
 node A (offline edits) ──┐
                          ├──POST /v1/sync/push──▶ [ arbitrate ] ──▶ federation_resources
 node B (offline edits) ──┘                              │              (authoritative)
                                                         │
                                                         ├──▶ federation_log (cursor)
                                                         │        │
                                                         │        └──GET /v1/sync/pull──▶ other nodes
                                                         │
                                                         └──▶ federation_conflicts (parked)
```

- **Authoritative state** — one row per federated record, keyed by
  `(resourceType, resourceId)`, carrying a `revision` that increments on every
  applied change.
- **Conflict detection** — every change declares the `baseRevision` the node
  believed it was editing. Equal to the current revision means a clean
  fast-forward; anything else means someone got there first.
- **Replication order** — `federation_log.cursor` is a database sequence, not a
  timestamp. Edge clocks drift, so ordering is assigned centrally and nodes pull
  "everything after cursor N".
- **Idempotency** — each change carries a node-generated `changeId`. A resend
  after a lost acknowledgement returns `duplicate` instead of applying twice.
- **Events** — `federation.change.applied`, `federation.conflict.detected`,
  `federation.conflict.resolved`, `federation.node.registered` on
  `iag.operations`, written through a transactional outbox so a change and its
  announcement commit together.

### What it deliberately does not do

It has no idea what a delivery note or a stock count *is*. Payloads are opaque
JSON; domain services keep ownership of meaning and this service owns only
convergence. It therefore cannot validate business rules — a node pushing
nonsense gets it stored faithfully.

It is also not a message broker or an API gateway: nothing routes *through* it
to another service.

## Conflict strategies

`CONFLICT_STRATEGY` selects the automatic policy. An invalid value fails at
boot rather than silently degrading to a different merge policy.

| Strategy | Behaviour | Use when |
|---|---|---|
| `last_write_wins` *(default)* | Later edit stamp wins; **ties keep the server** so the result does not depend on arrival order | Edits are naturally time-ordered |
| `server_wins` | Always keep the central record, reject the node's change | The centre owns the record |
| `node_wins` | Always apply the node's change | The edge is the system of record (e.g. physical stock counts) |
| `manual` | Park every conflict for a human; nothing is lost and nothing is guessed | Correctness matters more than throughput |

Two refinements apply regardless of strategy:

- A node re-pushing against **its own** already-superseded revision is a
  straggler, not a two-writer conflict — it is discarded without raising a
  conflict for a human.
- A change naming a `baseRevision` for a resource the centre has never seen is
  **not** treated as a fresh insert. That means state diverged, so it goes
  through conflict handling.

## API

All `/v1` routes require a Bearer token with `aud=iag.federation-gateway`.

| Method | Path | Permission |
|---|---|---|
| `GET` | `/v1/status` | `federation.view` |
| `POST` | `/v1/nodes/register` | `federation.sync` |
| `GET` | `/v1/nodes`, `/v1/nodes/:nodeId` | `federation.view` |
| `PATCH` | `/v1/nodes/:nodeId/status` | `federation.manage` |
| `POST` | `/v1/sync/push` | `federation.sync` |
| `GET` | `/v1/sync/pull?cursor=&limit=&nodeId=` | `federation.sync` |
| `POST` | `/v1/sync/ack` | `federation.sync` |
| `GET` | `/v1/resources/:type/:id` | `federation.view` |
| `GET` | `/v1/conflicts?state=pending` | `federation.view` |
| `POST` | `/v1/conflicts/:id/resolve` | `federation.resolve` |

`federation.sync` is deliberately separate from `federation.manage`: an edge
node's service account must push and pull continuously but must never be able
to suspend a sibling node or rewrite the conflict policy.

`/`, `/health`, `/healthz` and `/ready` are public probe paths.

### Push

```json
{
  "nodeId": "depot-kla-01",
  "changes": [
    {
      "changeId": "3f1c…",
      "resourceType": "delivery_note",
      "resourceId": "dn-1",
      "op": "upsert",
      "baseRevision": 4,
      "updatedAt": "2026-08-16T10:00:00Z",
      "payload": {"qty": 10}
    }
  ]
}
```

Each change gets its own result: `applied`, `duplicate`, `conflict_resolved`,
`conflict_pending` or `rejected`. A push is a batch for transport efficiency,
**not** an atomic unit — one parked conflict must not roll back the changes
that merged cleanly, so partial success is the normal outcome.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `PORT` / `ADDR` | `:4021` | Railway probes `PORT` |
| `DATABASE_URL` | — | **Required.** No memory mode: this service is the record of what every node has synced |
| `JWT_ISSUER`, `JWKS_URL` | `http://localhost:3001` | Point `JWKS_URL` at the private network; `JWT_ISSUER` must stay the public issuer string that auth signs into `iss` |
| `AUDIENCE` | `iag.federation-gateway` | Must appear in auth's `USER_TOKEN_AUDIENCES` |
| `SERVICE_CLIENT_ID` / `SERVICE_CLIENT_SECRET` | `iag-federation-gateway` | Must match the entry in auth's `SERVICE_CLIENT_SECRETS_JSON` |
| `CONFLICT_STRATEGY` | `last_write_wins` | See above |
| `MAX_PUSH_BATCH` / `MAX_PULL_BATCH` | `200` / `500` | Bounds lock-hold and response size |
| `AUTO_MIGRATE` | `true` | Must be `false` in production |
| `EVENT_BUS_ENABLED`, `KAFKA_BROKERS` | off | Events drop silently when disabled |

## Tests

```sh
go test ./...                       # merge rules, config, routing
TEST_DATABASE_URL=postgres://... go test ./internal/store/...   # real schema + queries
```

The store tests skip without `TEST_DATABASE_URL`, so CI without Postgres stays
green. They cover migration idempotency, the push/pull round-trip, `changeId`
replay, conflict park-then-resolve, transactional outbox writes, suspended-node
blocking, cursor monotonicity and deletes.

Registry: [`subrepos.json`](../../subrepos.json)
