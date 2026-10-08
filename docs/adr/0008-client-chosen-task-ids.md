# ADR 0008: Client-chosen task IDs

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

Producers often already have a stable identifier for the work they enqueue,
such as an export ID, `<workspace>.<job>` or `<run>-<attempt>`. They need to
read the task back by that identifier later (status, result, progress),
sometimes from another process that never saw the create response.

Today codeQ always generates the task ID, so these producers keep a
`their ID → codeQ ID` map next to the queue. That map can be lost, and it
drifts. Queues commonly let the client name the job instead: BullMQ
`jobId`, Cloud Tasks `name`, Sidekiq `jid`.

## Decision

A create accepts an optional `taskId` on:

- HTTP `POST /tasks`;
- each item of `POST /tasks/batch`;
- the producer stream (`CreateTask.task_id = 13`);
- `producerclient.CreateRequest.TaskID`.

When present, the task is created under that ID, so `GET /tasks/{taskId}`,
`/result`, worker routes and listings all use the client's identifier.

- **Shape.** The ID has 1–200 characters from `[A-Za-z0-9._:@+-]` and
  starts with a letter or digit (`domain.ValidTaskID`). `/` is excluded
  because storage keys end in `/<task id>`, and NUL because it separates
  binding namespaces. An invalid ID is rejected with `400`.
- **Existing ID.**
  - A create naming an existing task returns that task unchanged to a
    caller of the same tenant: a replay, `202` as for any create.
  - A caller of another tenant gets `409 {"error":"task_id_conflict"}`
    and neither the task nor its data. This follows the cross-tenant rule
    of `ErrIdempotencyConflict`.
  - Once a terminal task's body is reaped by retention, the ID can be used
    again.
- **Exclusive with `idempotencyKey`** (`400`). A named task is already
  idempotent by its ID; two keys deciding one create would contradict
  each other.

Implementation:

- `TaskRepository.EnqueueNamed` holds the ID's stripe (the
  `idempoStripes` added in 950ecbe, keyed by the task key) from the
  existence check to the commit, so two creates of one ID never both
  write.
- Generated IDs keep the previous path: no lookup and no lock.
- Routing follows the ID:
  - intra-process shards use `shardOf(taskID)`;
  - the cluster router sends the create to `ring.Owner(taskID)` with
    `EnqueueRequest.named = 11`, so the owner, not the router, performs
    the existence check. The conflict sentinel crosses the RPC like the
    idempotency one.

## Consequences

- Producers address tasks by their own identifiers without a side map, and
  a retried create with the same ID is safe.
- An ID is unique across tenants for as long as its task is retained. A
  tenant can learn that another tenant uses an ID (a `409`), never its
  content. This is the same exposure the idempotency key already has.
- Creates that name an ID pay one Pebble read and take a stripe. Creates
  without one are unchanged.

## Alternatives considered

- **Scope IDs per tenant (store `<tenant>:<id>`)**: removes the
  cross-tenant `409`, but every read route would have to rebuild the
  scoped key, and the public ID would no longer be what the client chose.
- **Answer an existing ID with `409` even for the same tenant (Cloud Tasks
  `ALREADY_EXISTS`)**: forces clients to special-case retries of their
  own create. Replay matches the idempotency key's contract.
- **A separate lookup index from client ID to generated ID**: two keys to
  keep consistent instead of one.

## References

- ADR 0003 for binding namespaces.
- `repository.ReplayIdempotent` for the cross-tenant rule.
