# Queueing model

## Tenant Isolation

All queues are isolated per tenant. Each tenant has dedicated queue structures that are completely independent from other tenants. The tenant ID is extracted from JWT claims and used to namespace all queue keys.

## Queue types

Each command and tenant combination is represented by a set of queues:

- Pending queue: list of IDs available for claim.
- In-progress queue: set of IDs currently leased (implemented as Redis SET for O(1) operations).
- Delayed queue: ZSET of IDs with `visibleAt` as score.
- Dead-letter queue: SET of IDs for tasks that exceeded `maxAttempts` (implemented as Redis SET for O(1) operations).

## Time-based scheduling

Delayed visibility is used for retries. The delayed ZSET score is `visibleAt`. Claim-time repair moves due tasks from delayed to pending.

## Priority scheduling

Priority is implemented using multiple pending lists per priority tier (0..9). The claim algorithm checks higher tiers first. This matches Dyno Queues support for priority queues while keeping list operations O(1).

Alternative: store ready tasks in a ZSET with score `(priority, sequence)` but that increases pop complexity to O(log n).

## Claim semantics

- A claim atomically pops one ID from pending and tracks it in in-progress via Lua (`RPOP` + `SADD`).
- The in-progress queue uses a Redis SET data structure for O(1) add and remove operations (`SADD`, `SREM`).
- A lease key is set with TTL `leaseSeconds` and value `workerId`.
- The task record is updated to `IN_PROGRESS`, `workerId`, and `leaseUntil`.
- Claims are tenant-scoped: workers can only claim tasks from their own tenant's queues.

### Claim-time repair (expired lease detection)

Before claiming a task, the system repairs expired leases using a sampling algorithm:

1. **Sample in-progress tasks**: Use `SRANDMEMBER` to randomly sample up to `inspectLimit` task IDs from the in-progress SET
2. **Check lease expiration**: Use pipelined `TTL` commands to efficiently check all sampled lease keys in a single round-trip
3. **Requeue expired tasks**: For tasks with expired leases (TTL ≤ 0), call `Nack()` to requeue with backoff or move to DLQ if max attempts exceeded

**Efficiency characteristics**:
- Time complexity: O(inspectLimit) for sampling and TTL checks
- Does not require scanning all in-progress tasks (which could be O(n) where n = total in-progress)
- Sampling provides probabilistic coverage: higher `inspectLimit` increases detection rate
- Default `inspectLimit = 200` balances repair coverage with claim latency

**Claim loop optimization**:
- The outer Go loop retries up to `inspectLimit` times to handle duplicate or invalid tasks
- The inner Lua script (`claimMoveScript`) checks only **1 task per invocation** to avoid O(n²) complexity
- This design keeps total work bounded at O(inspectLimit) rather than O(inspectLimit²)

## Ack and completion

Acknowledgement is equivalent to result submission. On success the task is removed from in-progress, the lease is cleared, and status is set to `COMPLETED` or `FAILED`. This follows the Dyno Queues model where unacknowledged messages are requeued after a timeout.

## Unack and retry

If the lease expires before completion or a worker sends `nack`, the task is retried. `attempts` is incremented on retry. If `attempts >= maxAttempts`, the task is moved to the dead-letter queue and marked `FAILED` with `error=MAX_ATTEMPTS`. Otherwise the task is moved to the delayed queue using the computed backoff delay.

## Idempotency

When `idempotencyKey` is provided, the service stores a mapping of `idempotencyKey -> taskId` with TTL equal to the retention window. Subsequent requests with the same key return the existing task.
