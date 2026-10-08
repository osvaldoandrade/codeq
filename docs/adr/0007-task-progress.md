# ADR 0007: Task progress

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

A long-running task (an export, a migration, a batch import) is opaque
between claim and result. The producer can only see `IN_PROGRESS`. It
cannot tell "processed 9,000 of 10,000 rows" from "stuck at row 3". Today
each team adds a side channel, usually a table or a cache key the worker
writes and the producer polls. That channel knows nothing about the lease,
so a worker that already lost the task keeps writing to it.

Other queues keep progress next to the job: BullMQ has
`job.updateProgress(value)`, and Celery has
`task.update_state(state='PROGRESS', meta=...)`. In both, the value is
free-form and is read back with the job.

## Decision

The worker that holds a task's lease can store one free-form JSON progress
value on the task. Anyone allowed to read the task sees it in
`GET /v1/codeq/tasks/{id}`.

**API.** `POST /v1/codeq/tasks/{id}/progress` with body
`{"progress": <any JSON value>}`.

- The route is in the worker group. It requires the `codeq:heartbeat`
  scope and has the same leader forwarding (`fwd.Single()`) as heartbeat.
  Progress is a liveness-class report by the lease holder, so existing
  worker tokens can send it without being reissued.
- The body is decoded strictly: one object, `progress` is the only field,
  and `progress` must be present and not `null`. Anything else is `400`.
- The value is compacted. If it is larger than 64 KiB after compaction,
  the request fails with `413 {"error":"progress too large"}`. A body
  larger than 1 MiB fails the same way before it is parsed. 413 was chosen
  over 400 because the request is well formed and only its size is
  refused.
- The response is `200 {"ok":true}`.

**Who may write.** The rules are the ones heartbeat uses, plus the state
check that nack and abandon use:

| Condition | Status |
|---|---|
| The caller is not the task's `workerId` (also after nack, abandon, completion or lease expiry) | `403 not-owner` |
| The task does not exist | `404 not-found` |
| The caller matches but the task is not `IN_PROGRESS` | `409 not-in-progress` |

- Heartbeat keeps its current mapping. Only the new route maps
  `not-found` to `404`.
- Binding-scoped tokens (ADR 0003): the route is on the allow-list for
  `Subscribe` only, like heartbeat. The same strict owner pre-check runs,
  so a missing task and a foreign task both answer `403 not-owner`.

**Storage.**

- `domain.Task` gains `Progress json.RawMessage` (`json:"progress,omitempty"`).
  Task bodies without progress serialize exactly as before.
- `TaskRepository.Progress` rewrites the task body in a single Pebble
  batch through `CommitBatch`. In Raft mode that is the replicated write
  path, with the same `ensureLeaderDispatch` check as heartbeat. The lease,
  the queue indexes and the TTL index are not touched.
- No transition clears progress. It survives nack, abandon, lease expiry
  and retries, so the next attempt and the producer see the last reported
  value. BullMQ behaves the same way.

**Repository modes.**

- `ShardedTaskRepository` routes by `shardOf(taskID)`.
- `cluster.TaskRouter` writes locally when the ring owns the ID. Otherwise
  it calls the new `TaskNode.Progress` RPC (`ProgressRequest`/
  `ProgressResponse`, with the same `not_found`/`not_owner`/
  `not_in_progress` flags as `Abandon`).
- `clusterpb.Task` carries `bytes progress = 21`, so a task read through a
  peer keeps its progress. Field 20 is `deduplication_key`.

**Service.** `SchedulerService.ReportProgress` passes the value through to
the repository. The HTTP layer validates it.

## Consequences

- Producers and dashboards can show how far a long task has got, without
  a side channel. A lost lease stops further writes from that worker.
- Each report rewrites the whole task body. The 64 KiB cap bounds the
  extra write and replication cost. Workers should report on a cadence
  (for example every few seconds or every N items), not per item.
- The value is last-write-wins and has no history. It is not a log.
- Progress uses the read-modify-write sequence that heartbeat already
  uses. The repository has no per-task lock. A report that races a nack,
  abandon or result on the same task has the same exposure that heartbeat
  has today. Fixing this for both belongs in one later change.
- Mixed-version clusters: the cluster-mode RPC is new, so during a rolling
  upgrade of a static ring, a report routed to a node on an older build
  fails with gRPC `Unimplemented` (HTTP `500`). An older build that
  rewrites a task body (heartbeat, nack, claim) also drops a progress value
  it does not know. Raft groups replicate opaque batches and are not
  affected.
- Out of scope, follow-ups:
  - parity on the gRPC worker stream (`workerpb`), which needs a new
    `WorkerEvent` variant;
  - progress in result webhooks;
  - support in `pkg/workerclient`.

## Alternatives considered

- **A dedicated `codeq:progress` scope**: every deployed worker token
  would need to be reissued before workers could report. Progress carries
  no more authority than heartbeat, which already lets the holder keep the
  task alive.
- **Fold progress into heartbeat**: changes an existing contract and
  couples the lease cadence to the reporting cadence.
- **A separate progress key outside the task body**: needs a second read
  on `GET /tasks/{id}`, a second key per task in every repository mode and
  in the TTL cleanup, and a second proto read path. The value is small
  and read with the task, so it belongs in the body.
- **Clear progress on nack or retry**: the next attempt loses the
  information it needs to resume. BullMQ keeps progress across retries.
- **Typed progress (`{current, total}`)**: too narrow for what callers
  report (percentages, stages, row counts). The free-form value matches
  BullMQ and Celery.

## References

- ADR 0003 for binding-scoped tokens and the strict owner pre-check.
- BullMQ `Job.updateProgress`: https://docs.bullmq.io/
- Celery custom states (`update_state`):
  https://docs.celeryq.dev/en/stable/userguide/tasks.html#custom-states
