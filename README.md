# iag-federation-gateway

Central **sync and conflict resolution** for federated IAG deployments.

Edge nodes — depot nodes, POS terminals, field apps — capture and mutate records
while offline. Two nodes can edit the same record during the same offline
window, so bringing them back together is not a copy: it is a **merge with an
explicit conflict policy**. This service is the authority that performs it.

Headless Go/Gin JSON API — no bundled UI.

## How it works

```
  node A ──┐                                    ┌──▶ federation_resources  (authoritative state)
           ├── POST /v1/sync/push ──▶ [ merge ] ─┼──▶ federation_log        (ordered replication log)
  node B ──┘                              │      └──▶ federation_conflicts  (parked divergences)
                                          │
  node A ◀── GET /v1/sync/pull?cursor= ───┘
```

- **Resources** — one authoritative row per `(resourceType, resourceId)` with a
  `revision` counter. The payload is opaque JSON: the gateway arbitrates
  convergence, domain services keep ownership of meaning.
- **Conflict detection** — every pushed change carries the `baseRevision` the
  node believed it was editing. Matching the current revision is a clean
  fast-forward; anything else means someone else edited it first.
- **Replication order** — the log cursor is a database sequence, **not** a
  timestamp. Edge clocks drift, so ordering is assigned centrally and a node
  can never miss or reorder changes.
- **Idempotency** — each change carries a node-generated `changeId`. A resend
  after a lost ack returns `duplicate` instead of applying twice.
- **Events** — emits `federation.change.applied`,
  `federation.conflict.detected`, `federation.conflict.resolved` and
  `federation.node.registered` on `iag.operations`, via a transactional outbox
  so a change and its announcement commit together.

## Conflict strategies

Set `CONFLICT_STRATEGY`. An unrecognised value **fails the boot** rather than
silently degrading to a different merge policy.

| Strategy | Behaviour | Use when |
|---|---|---|
| `last_write_wins` (default) | The later edit stamp wins; ties keep the server copy | Edits are naturally sequential |
| `server_wins` | The central record always survives | The centre owns the record |
| `node_wins` | The node's change always applies | The edge is the system of record (e.g. physical stock counts) |
| `manual` | Every conflict is parked for a human | Nothing may be guessed |

Two refinements worth knowing:

- **Ties keep the server copy.** With equal timestamps there is no evidence the
  node's edit is newer, so preferring the incumbent makes the result
  deterministic instead of dependent on which request arrived first.
- **Same-node stragglers are not conflicts.** A node re-pushing against its own
  already-superseded revision is discarded, not parked — it is not a
  two-writer divergence and must not generate queue noise for an operator.

## API

All `/v1` routes require a Bearer token with `aud=iag.federation-gateway`.

| Method | Path | Permission |
|---|---|---|
| GET | `/v1/status` | `federation.view` |
| POST | `/v1/nodes/register` | `federation.sync` |
| GET | `/v1/nodes`, `/v1/nodes/:nodeId` | `federation.view` |
| PATCH | `/v1/nodes/:nodeId/status` | `federation.manage` |
| POST | `/v1/sync/push` | `federation.sync` |
| GET | `/v1/sync/pull?cursor=&limit=&nodeId=` | `federation.sync` |
| POST | `/v1/sync/ack` | `federation.sync` |
| GET | `/v1/resources/:type/:id` | `federation.view` |
| GET | `/v1/conflicts?state=pending` | `federation.view` |
| POST | `/v1/conflicts/:id/resolve` | `federation.resolve` |

`federation.sync` is deliberately separate from `federation.manage`: an edge
node's service account must push and pull continuously, but must never be able
to suspend a sibling node or rewrite the conflict policy.

`/`, `/health`, `/healthz` and `/ready` are public probes.

### Pushing changes

```jsonc
POST /v1/sync/push
{
  "nodeId": "depot-kla-01",
  "changes": [{
    "changeId": "9f1c…",        // node-generated uuid; makes the push idempotent
    "resourceType": "delivery_note",
    "resourceId": "dn-1024",
    "op": "upsert",              // or "delete"
    "baseRevision": 3,           // 0 when the node believes the record is new
    "updatedAt": "2026-08-16T09:12:00Z",
    "payload": { }
  }]
}
```

Each change comes back with one of: `applied`, `duplicate`, `conflict_resolved`,
`conflict_pending`, `rejected` — plus the resulting `revision` and `cursor`.
A push is a batch for transport efficiency, **not** an atomic unit: partial
success is expected, and one parked conflict must not roll back changes that
merged cleanly.

## Auth

Inbound Bearer+aud, verified locally against JWKS. RBAC codenames
(`federation.view|sync|resolve|manage`) are registered with iag-authentication
at boot, so `SERVICE_CLIENT_ID` must appear in that service's
`SERVICE_CLIENT_SECRETS_JSON` with a matching secret.

## Configuration

See [`config/.env.example`](config/.env.example).

## Local development

```sh
go test ./...
go run .
```

Registry: [`subrepos.json`](../../subrepos.json)
