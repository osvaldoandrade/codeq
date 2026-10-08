# ADR 0006: Recurring schedules

- **Status**: Proposed
- **Date**: 2026-10-08
- **Deciders**: @osvaldoandrade (maintainer); proposed by @fernandomarcius
- **Tracks**: —

## Context

Producers that run periodic work today keep an external cron (a Kubernetes
CronJob, a sidecar, an in-process timer) whose only job is to call
`POST /tasks` on time. Each of them re-solves the same problems, usually
badly. With more than one replica, every replica fires. A crash between
"timer fired" and "task created" loses a run. A failover can double one.
Queues that offer repeatable jobs (BullMQ job schedulers, Sidekiq-cron,
Celery beat) keep the rule next to the queue, so exactly one task is
enqueued per slot.

codeQ already has the two pieces this needs. Writes replicate through Raft
with one leader per group. Since 950ecbe, create-with-idempotency-key is
serialized, so a repeated key returns the first task.

## Decision

A tenant-scoped catalog of recurring schedules. The leader of the first
shard fires each schedule's slots by creating tasks through the scheduler.

**API** (admin group, `RequireAdmin`; the tenant always comes from the token):

| Route | Effect |
|---|---|
| `PUT /v1/codeq/admin/schedules/{name}` | Create (`201`), replace (`200`, bumps `version`), or confirm an identical spec (`200`, no change). |
| `GET /v1/codeq/admin/schedules/{name}` | One schedule. |
| `GET /v1/codeq/admin/schedules` | Every schedule of the tenant, by name. |
| `DELETE /v1/codeq/admin/schedules/{name}` | Delete; also `204` when absent. |

The spec is
`{cron, timezone, command, payload, priority, maxAttempts, webhook, paused}`.

- `cron` is the five-field standard dialect plus descriptors (`@hourly`,
  `@every 30s`). This is the Kubernetes CronJob dialect, parsed by
  `github.com/robfig/cron/v3`.
- `timezone` is an IANA name, UTC by default. An embedded `CRON_TZ=` is
  refused so there is one place for the zone.
- A schedule exposes `nextRunAt`, `lastRunAt` and `lastTaskId`.

**Exactly one task per slot.** For each due slot the runner calls
`SchedulerService.CreateTask` with the idempotency key
`NUL "schedule" NUL <tenant>.<name> NUL <slot RFC3339>` and only then
records the slot with `Advance`, a compare-and-set on
`(version, nextRunAt)`.

- A crash, lost leadership or failed write between the two steps makes the
  slot fire again. The new attempt resolves to the task already created,
  on this node or on the next leader, because the key and the task are in
  the same replicated batch.
- Client keys cannot contain NUL, and binding keys never start with it, so
  a slot key cannot collide with either.

**Where it runs.**

- The catalog lives on shard 0 (`codeq/admin/schedules/<tenant>.<name>`),
  like the topic catalog.
- The runner ticks every second on the node that leads shard 0 (every
  node when Raft is off).
- Scheduled tasks are created on shard 0 as well, through
  `ShardedTaskRepository.OnShard(0)` when there are several shards. The
  runner therefore writes only to the group it leads, whatever the
  leaders of the other shards are.

**Timing semantics.**

- A slot fires once its time has passed on the leader's clock. The
  observed lag is in `codeq_schedule_fire_delay_seconds`, normally under
  one tick.
- After downtime, the earliest missed slot fires once, and the schedule
  resumes from the first slot after now. There is no catch-up burst.
- Updating a spec keeps `nextRunAt` when the timing (cron, timezone,
  paused) is unchanged, and recomputes it from the update time otherwise.
  Resuming a paused schedule never catches up the paused period.

**Rollout and modes.**

- Raft: the routes answer `503` and the runner does not start until every
  peer runs a compatible build and `raft.scheduleCatalogProtocol=v1`
  (`RAFT_SCHEDULE_CATALOG_PROTOCOL`) is set. This is the same gate as
  `raft.topicCatalogProtocol`.
- Cluster mode (static ring without Raft) keeps one catalog per node, so
  it cannot fire exactly once: schedules answer `503` there.

**Observability.** `codeq_schedule_fires_total{outcome}` counts slots as
`enqueued`, `failed` (retried on the next tick) or `superseded` (the spec
changed while firing).

## Consequences

- Periodic producers lose their external cron, and replicas or failovers
  no longer duplicate or skip runs.
- All scheduled tasks start on shard 0. Schedules are low-volume, and
  claims already scan every shard, so this is a placement choice and not
  a throughput limit for normal use.
- The runner scans the catalog every second (`O(schedules)`). That is
  fine for thousands of schedules. A next-run index can come later
  without changing the API.
- One new dependency: `github.com/robfig/cron/v3` v3.0.1. It is MIT, has
  no transitive dependencies, and is the parser behind Kubernetes
  CronJob.
- New config: `raft.scheduleCatalogProtocol`. It is required only in Raft
  mode.

## Alternatives considered

- **Fire from every node, deduplicated by the slot key alone**: correct,
  but N nodes do the work of one and contend on the same stripe every
  second.
- **Store a next-run index and fire from the FSM**: the FSM replays opaque
  batches by design; conditional logic there would be a new replication
  protocol.
- **Write a cron parser in-tree**: more code to review for no gain over a
  stable, widely used parser; DST and descriptor handling are where
  hand-written parsers fail.
- **Catch up every missed slot**: turns an outage into a burst of
  identical work. Queues that support it (BullMQ, Celery beat) default to
  not doing it.
- **Enqueue on the shard of each task ID**: the runner would depend on
  other shards' leaders. In multi-group Raft that can stall firing while
  leadership is split.

## References

- ADR 0003 for binding tokens and the idempotency namespace.
- 950ecbe for serialized idempotent creates.
- Kubernetes CronJob schedule syntax:
  https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/#schedule-syntax
