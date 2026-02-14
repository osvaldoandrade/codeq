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
- `workerId`: current owner
- `leaseUntil`: advisory timestamp
- `resultKey`: link to the result record
- `createdAt`, `updatedAt`
- `attempts`: incremented on each claim
- `maxAttempts`: policy limit

## Result

A Result record stores completion data:

- `taskId`
- `status`: `COMPLETED` or `FAILED`
- `result`: JSON object
- `error`: error string
- `artifacts`: array of `{name, url}`
- `completedAt`

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

Sharding is a storage-level partition and does not change the API surface. It is not implemented in the current service; this section is reserved for a future expansion.

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
