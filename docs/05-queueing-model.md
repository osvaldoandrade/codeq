# Queueing model

## Queue types

Each command is represented by a set of queues:

- Pending queue: list of IDs available for claim.
- In-progress queue: list of IDs currently leased.
- Delayed queue: ZSET of IDs with `visibleAt` as score.
- Dead-letter queue: list or ZSET for tasks that exceeded `maxAttempts`.

## Time-based scheduling

Delayed visibility is used for retries. The delayed ZSET score is `visibleAt`. Claim-time repair moves due tasks from delayed to pending.

## Priority scheduling

Priority is implemented using multiple pending lists per priority tier (0..9). The claim algorithm checks higher tiers first. This matches Dyno Queues support for priority queues while keeping list operations O(1).

Alternative: store ready tasks in a ZSET with score `(priority, sequence)` but that increases pop complexity to O(log n).

## Claim semantics

- A claim moves one ID from pending to in-progress using `RPOPLPUSH`.
- A lease key is set with TTL `leaseSeconds` and value `workerId`.
- The task record is updated to `IN_PROGRESS`, `workerId`, and `leaseUntil`.

## Ack and completion

Acknowledgement is equivalent to result submission. On success the task is removed from in-progress, the lease is cleared, and status is set to `COMPLETED` or `FAILED`. This follows the Dyno Queues model where unacknowledged messages are requeued after a timeout.

## Unack and retry

If the lease expires before completion or a worker sends `nack`, the task is retried. `attempts` is incremented on retry. If `attempts >= maxAttempts`, the task is moved to the dead-letter queue and marked `FAILED` with `error=MAX_ATTEMPTS`. Otherwise the task is moved to the delayed queue using the computed backoff delay.

## Idempotency

When `idempotencyKey` is provided, the service stores a mapping of `idempotencyKey -> taskId` with TTL equal to the retention window. Subsequent requests with the same key return the existing task.
