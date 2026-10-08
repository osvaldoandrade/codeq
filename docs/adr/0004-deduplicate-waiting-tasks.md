# ADR 0004: Deduplicate creates while a task waits

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

Producers often enqueue "do X for entity E" from many places: a webhook per
inbound event, a timer, a user action. While one such task still waits, a
second one adds no work, only load: the worker that runs the first already
sees the latest state of E. Queues commonly answer this with a deduplication
key that is held while the job waits and released once it starts (BullMQ
`deduplication`, Sidekiq `unique_until: :start`, Cloud Tasks named tasks).

codeQ has `idempotencyKey`, which answers a different question: "was this
exact request already accepted?". It maps the key to the task for as long as
the task body is retained, whatever its status, so it cannot express "run
again after the current one started". Producers that need the waiting-only
semantics today must keep their own index next to codeQ, which is racy and
duplicates queue state.

## Decision

A create accepts an optional `deduplicationKey` (HTTP `POST /tasks`, each item
of `POST /tasks/batch`, producer gRPC `CreateTask.deduplication_key`, and
`producerclient.CreateRequest.DeduplicationKey`).

- **Scope.** The key is scoped to (tenant, command, key). The command is
  compared case-insensitively, as queue keys already are. The tenant comes
  from the validated token, so a key never crosses tenants.
- **Join while waiting.** If a task of the same scope holds the key and is
  still waiting (status `PENDING`, ready or delayed), the create writes
  nothing and returns that task (`202`, the same response shape as a create).
  The first task wins: the payload, priority and schedule of the later create
  are discarded.
- **Release on claim.** The claim that moves the task to `IN_PROGRESS`
  releases the key in the same Pebble batch (`Claim` and `ClaimMany`). The next
  create of the key enqueues a new task, which later creates join in turn.
  Retries of the running task (nack, lease expiry, a dead-letter requeue)
  do not re-acquire the key, so a release deletes the mapping only while it
  still names the released task: a newer task may hold the key by then. An
  admin delete of a waiting task (ADR 0009) releases the key the same way.
- **Exclusive with idempotency.** A create with both `idempotencyKey` and
  `deduplicationKey` fails with `400` (`domain.ErrDeduplicationWithIdempotency`):
  each key alone decides whether the create writes a task.

Storage: `codeq/dedupe/<tenant, command, key>` → task ID, with each component
length-prefixed (`pebble.KeyDedupe`) so no tuple encodes to another. The
mapping is written in the create batch and deleted in the claim batch, so it
replicates through the existing Raft batch path and is covered by snapshots
(`codeq/` range). A create holds one of 256 striped mutexes for its key from
the lookup to the commit, so two concurrent creates of one key never both
write. The lookup also re-reads the mapped task and treats a missing or
non-waiting task as a free key, so a stale mapping can never block or misroute
a create.

Routing keeps every create of one key on one writer: intra-process shards
place the task on `shardOf(KeyDedupe(...))`, and the cluster router places it
on the node that owns that key (`Ring.GenerateOwnedID`), carrying the key in
`clusterpb.EnqueueRequest` and `clusterpb.Task`.

## Consequences

- Producers get waiting-only deduplication without an external index, under
  the same tenant isolation as every queue key.
- Creates with a key serialize per key on the leader; creates without a key
  are unchanged (no lock, no extra read).
- `domain.Task` gains `deduplicationKey` (omitted when empty), so task reads
  show which key a task holds.
- A mapping whose task body is removed by the TTL reaper while still waiting
  stays on disk until the next create of the key replaces it: at most one
  entry per distinct key, never a correctness issue.
- Rolling upgrades need no protocol gate. Old peers replay the new keys as
  plain Pebble writes. While an old peer leads, creates ignore the key (no
  deduplication, never a lost task), and its claims leave the mapping, which
  the next create on a new leader treats as stale.

## Alternatives considered

- **Reuse `idempotencyKey` with a "release on claim" flag**: overloads one
  field with two lifetimes and changes the meaning of existing replays.
- **Replace the waiting task's payload with the latest one (BullMQ
  `keepLastIfActive` + replace)**: needs a rewrite of the task body under the
  same lock and a policy flag; first-wins covers the common "coalesce
  triggers" case, and replace can be added later without breaking this API.
- **Do the check inside the Raft FSM**: the FSM replays opaque Pebble batches
  by design; conditional logic there would be a new replication protocol.
- **Client-side index (e.g. Redis `SET NX`)**: races with claims, which only
  codeQ observes atomically.

## References

- BullMQ deduplication: https://docs.bullmq.io/guide/jobs/deduplication
- ADR 0003 (binding-scoped tokens) for the tenant/command scoping that
  binding tokens already enforce on create.
