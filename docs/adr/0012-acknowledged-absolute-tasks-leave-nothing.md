# ADR 0012: An acknowledged absolute task leaves nothing behind (CFP-070)

- **Status**: Accepted
- **Date**: 2026-10-29
- **Deciders**: Osvaldo Andrade (owner go, 2026-10-29: "sim, vamos fazer e subir")
- **Amends**: ADR 0010 (replay boundary), for acknowledged tasks only

## Context

`absoluteExpiry` topics exist for Managed Logs (`<tenant>.cf-logs`). Under ADR
0010 a completed task kept its payload and result until its custody deadline,
up to 30 minutes. Its identity then stayed as a 48-hour replay tombstone. Logs
complete in seconds, so the queue's replicated state was almost entirely
acknowledged batches. On CODE that was about 300 MB, and every voter keeps two
snapshots of it plus one in progress, which filled the 1 GiB voter volumes on
2026-10-24, 2026-10-26 and 2026-10-28.

The consumer already owns the guarantees the tombstone was meant to give. The
Logs worker writes each record with a conditional put on a deterministic key.
It also keeps a per-event receipt in CefasDB with the original acceptance and
expiry, for 48 hours after that expiry. A re-sent record is therefore never
duplicated, and a record whose retention ended is never revived. A replay
tombstone in the queue after acknowledgement adds nothing.

## Decision

- When a worker submits `COMPLETED` for an `absoluteExpiry` task,
  `ResultRepository.UpdateTaskOnComplete` retires it entirely
  (`DB.retireCompletedAbsolute`). One replicated batch, under the absolute
  lock, deletes the task, result and lease, every reverse-referenced
  queue/TTL/dedupe key, and the identity with its topic index. It also
  decrements the identity count. No key or value mentioning the ID remains.
- Publishing the same ID after acknowledgement creates a new task. Any
  consumer of an absolute topic must make processing idempotent, as Managed
  Logs does.
- `FAILED`, nack, DLQ, lease expiry and deadline retirement are unchanged.
  They keep custody until the deadline and then the bounded 48-hour tombstone
  of ADR 0010.
- Metric `codeq_absolute_completed_purged_total` counts these retirements.

## Consequences

- The queue holds Logs payloads only while they are in flight. Steady-state
  identities fall from about 48 hours of batches to the in-flight backlog, and
  snapshots shrink with them. Tombstones written before the upgrade drain
  within their recorded 48-hour windows.
- A late `GET` of an acknowledged absolute task or of its result returns not
  found. The Logs worker never reads either after acknowledging.
- There is no wire, format or configuration change, and rolling upgrade is
  safe. The leader decides, and followers apply the same raw batch. Rollback
  restores ADR 0010 behaviour for tasks completed afterwards.

## Alternatives considered

- **A new per-topic `purgeOnCompletion` policy:** it would need the QueueTopic
  CRD, the controller and API chain for behaviour that only `absoluteExpiry`
  topics need. Rejected.
- **Shortening the tombstone window:** it still retains state for every
  acknowledged batch. Rejected.
- **Carrying references instead of payloads (claim check):** a larger
  redesign of the Logs pipeline. Not needed once acknowledged batches are
  removed.

## References

- ADR 0010; root `docs/adr/0023-managed-kubernetes-logs.md`;
  code-admin-logs-runtime `internal/cefas/receipt.go` (per-event receipts) and
  `internal/worker/worker.go` (acknowledgement).
- Root `docs/rollouts/managed-logs-code-first.md`, checkpoints of 2026-10-28
  and 2026-10-29.
