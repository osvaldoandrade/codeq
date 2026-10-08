# ADR 0005: List the tasks of a queue state

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

`GET /admin/queues/{command}` returns how many tasks a queue holds in each
state (ready, delayed, in progress, DLQ), but there is no way to see which
tasks they are. Operators inspecting a stuck queue, and producers that
reconcile their own records against queued work (for example "which of my
workflow runs still have a task in flight?"), have to keep a parallel index
of task IDs next to codeQ, which drifts from the queue as soon as a claim,
nack or lease expiry happens.

The Pebble keyspace already keeps one ordered index per (command, tenant,
state): `pending/<prio>/<seq>/<id>`, `delayed/<score>/<id>`, `inprog/<id>` and
`dlq/<id>`. Listing is a range scan over those indexes.

## Decision

`GET /v1/codeq/admin/queues/{command}/tasks?state=…&limit=…&cursor=…` returns
one page of the tasks of the caller's tenant in that queue state:

```json
{"tasks": [ {task}, … ], "nextCursor": "…"}
```

- **States** (`domain.QueueState`) use the `QueueStats` vocabulary: `ready`
  (claim order: highest priority first, FIFO within a priority), `delayed`
  (earliest visible time first), `inProgress` (task ID order) and `dlq` (task
  ID order). `state` is required.
- **Pages**: `limit` defaults to 100 and accepts 1–500, since every task carries
  its payload. `nextCursor` is opaque and absent on the last page. A page can
  hold fewer tasks than `limit` (stale index entries are skipped), and a page
  that ends exactly at the end of a partition may be followed by an empty last
  page.
- **Authorization**: the route sits in the existing `admin` group
  (`RequireAdmin`). It is not added to the topic-controller allow-list,
  because a listing exposes payloads and a count does not. The tenant comes
  only from the validated token.
- **Consistency**: a page is read from the local Pebble state without a leader
  hop, like `GET /tasks/{id}`. Each listed task's body is re-read, and an
  entry whose task no longer has the state's status is skipped, so a page
  never shows a task in a state it has left.

Implementation:

- `repository.TaskRepository.ListTasks` with three implementations.
  - `pebble.TaskRepository` scans the index spans. Its cursor is the last
    index key consumed; a cursor outside the listed (tenant, command, state)
    spans fails with `domain.ErrInvalidCursor`, so a crafted cursor cannot
    reach another tenant's keys.
  - `ShardedTaskRepository` and `cluster.TaskRouter` walk their partitions
    (shards, ring nodes) in order through `repository.ListAcrossPartitions`.
    Its cursor names the partition to resume in plus that partition's own
    cursor.
- The cluster adds a `ListTasks` RPC to `TaskNode`. A node that cannot answer
  fails the page rather than being skipped: a caller listing in-flight work
  must not mistake an unreachable node for an empty one.

## Consequences

- Producers and operators can enumerate a queue state without a side index.
- There is still no listing of COMPLETED tasks, or of tasks FAILED through a
  result submit: those have no index, and adding one would cost a write on
  every completion. `GET /tasks/{id}` remains the way to read them.
- A cursor stays valid while the set of partitions is unchanged; after a shard
  or ring change a cluster cursor may fail with `400` and the listing restarts.
- `pkg/persistence.TaskStorage` (the public plugin interface) is unchanged, so
  third-party plugins keep compiling. The server's storage contract,
  `repository.TaskRepository`, gains the method.

## Alternatives considered

- **Offset pagination**: re-scans from the start on every page, and skips or
  repeats tasks as claims move them.
- **A secondary index by status for every task**: lists completed tasks too,
  but adds writes on the hot completion path; the existing per-state indexes
  already answer the open states.
- **Make the route producer-scoped instead of admin**: lets any producer token
  read every payload of its tenant's queue; the admin scope is the
  conservative default and can be widened later.
- **Skip unreachable cluster nodes, like `QueueStats`**: acceptable for an
  approximate count, wrong for an enumeration.

## References

- ADR 0001 (target architecture) for the repository and transport layering.
