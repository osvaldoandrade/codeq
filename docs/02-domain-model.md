# Domain model

This section describes the core domain layer used by codeQ. Names and responsibilities align with Dyno Queues concepts such as `Message` and `ShardSupplier`.

## Message

A Message is the unit stored in queues. It is distinct from a Task record and contains scheduling metadata.

Required fields:

- `id` (string): UUID v4 assigned by codeQ.
- `command` (string): routing key (aka event type).
- `payload` (string): JSON string.
- `priority` (int): higher value means higher priority.

Optional fields:

- `headers` (map[string]string): small metadata for routing or tracing.
- `receiveCount` (int): number of delivery attempts.
- `traceId` (string): request correlation.
- `scheduledAt` (RFC3339): when the message was enqueued.

In codeQ, the Message is serialized into the Task record. Delayed visibility is tracked in the delayed queue score rather than a `visibleAt` field on the record.

## Task

A Task is a stateful record stored in the task hash. It includes:

- `id`, `command`, `payload`, `priority`
- `status`: `PENDING`, `IN_PROGRESS`, `COMPLETED`, `FAILED`
- `lastKnownLocation`: hint for targeted cleanup (`PENDING_LIST`, `DELAYED_ZSET`, `INPROG_SET`, `DLQ_SET`, `NONE`)
- `workerId`: current owner
- `leaseUntil`: advisory timestamp
- `resultKey`: link to the result record
- `tenantId`: tenant identifier for multi-tenant isolation
- `createdAt`, `updatedAt`
- `attempts`: incremented on each claim
- `maxAttempts`: policy limit

The `tenantId` field enables complete queue isolation in multi-tenant deployments. It is automatically populated from JWT claims during task creation and used to namespace all queue operations. For single-tenant deployments, this field contains the JWT subject identifier.

The `lastKnownLocation` field is an optimization hint that tracks where a task was last placed in the queue system. This allows the admin cleanup operation to avoid expensive O(N) list scans when removing tasks. The field is not authoritative and may be out of sync if tasks are moved by external processes.

### TaskLocation enum

The `TaskLocation` field tracks task placement for administrative cleanup optimization:

- `PENDING_LIST`: Task is in the ready queue (Redis LIST)
- `DELAYED_ZSET`: Task is in the delayed queue (Redis ZSET)
- `INPROG_SET`: Task is in the in-progress queue (Redis SET)
- `DLQ_SET`: Task is in the dead letter queue (Redis SET)
- `NONE`: Location unknown or task completed

This tracking enables O(1) task removal during cleanup by targeting the specific data structure instead of scanning all queues.

## Result

A Result record stores completion data:

- `taskId`: Reference to the completed task
- `status`: `COMPLETED` or `FAILED`
- `result`: JSON object containing task output
- `error`: Error message string (for failed tasks)
- `artifacts`: Array of artifact references with structure `{name, url}`
- `completedAt`: RFC3339 timestamp of completion

### Artifact structure

Result artifacts support attaching files or external resources to task results:

- `ArtifactIn` (request): `{name: string, content: base64-encoded-string}` - Submitted by workers
- `ArtifactOut` (response): `{name: string, url: string}` - Returned by API with storage URL

Artifacts are stored locally by default (see `localArtifactsDir` config). For production multi-replica deployments, integrate external object storage.

## Subscription

A Subscription represents a webhook listener for worker availability or task events:

- `id` (string): UUID v4 assigned by codeQ
- `callbackUrl` (string): HTTP endpoint for event notifications
- `eventTypes` ([]Command): list of commands to subscribe to
- `deliveryMode` (string): delivery strategy (e.g., `broadcast`, `group`)
- `groupId` (string, optional): for grouped delivery mode
- `minIntervalSeconds` (int): minimum time between notifications
- `expiresAt` (RFC3339): subscription expiration timestamp
- `createdAt` (RFC3339): subscription creation timestamp

Subscriptions are used for worker availability webhooks (see `docs/12-webhooks.md`).

## QueueStats

QueueStats provides queue depth metrics for a specific command:

- `command` (Command): the command/queue name
- `ready` (int64): tasks available for claiming
- `delayed` (int64): tasks scheduled for future delivery
- `inProgress` (int64): tasks currently claimed by workers
- `dlq` (int64): tasks in the dead letter queue (exceeded maxAttempts)

These metrics are returned by the queue admin API (`GET /v1/codeq/admin/queues`).

## ShardSupplier

`ShardSupplier` provides a mapping from commands to shard identifiers and defines the current shard used for queue operations. This mirrors Dyno Queues which exposes `getQueueShards` and `getCurrentShard`.

Suggested interface (Go):

```go
// ShardSupplier maps commands to shard identifiers.
type ShardSupplier interface {
    // QueueShards returns the shard list for a command.
    QueueShards(command string) []string
    // CurrentShard returns the shard to use for claim operations.
    CurrentShard(command string) string
}
```

Sharding is a storage-level partition and does not change the API surface. It is designed but not yet implemented in the current service. For the complete sharding design, architecture diagrams, and implementation roadmap, see **[Queue Sharding HLD](24-queue-sharding-hld.md)** and **[Sharding Status](06-sharding.md)**.

## Worker token

The worker identity is derived from JWT `sub`. codeQ does not store worker records. The token includes `eventTypes` used for authorization.

## Queue API (internal)

The internal queue interface mirrors Dyno Queues semantics with push, pop, and ack. A pop that is not acknowledged within the lease window is requeued.

Suggested interface (Go):

```go
type Queue interface {
    Push(ctx context.Context, msg Message) error
    Pop(ctx context.Context, command string) (*Message, error)
    Ack(ctx context.Context, messageID string) error
    Size(ctx context.Context, command string) (int64, error)
}
```
