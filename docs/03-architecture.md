# Architecture and flow

## Components

- HTTP API: Gin-based router with JSON binding.
- Auth: producer token validation via Identity, worker token validation via JWKS.
- Scheduler core: orchestrates queue and task state transitions.
- Result processor: validates completion payloads and stores results.
- Storage: KVRocks via Redis API.
- Artifact storage: local filesystem uploader.
- Notifier: optional webhook signal dispatcher.
- Requeue loop: claim-time repair during `Claim`.

## Enqueue flow

1. Producer submits `command`, `payload`, `priority`, and optional `webhook`.
2. Service validates fields and normalizes the payload to a JSON string.
3. Service writes the task record and inserts the task ID into the pending list.
4. Service updates the retention index.

## Claim flow (pull)

1. Worker submits claim request with `commands` and optional `leaseSeconds`.
2. Service validates token and filters event types by token claims.
3. Service runs the requeue logic for each command.
4. Service moves one ID from pending list to in-progress list using `RPOPLPUSH`.
5. Service sets a lease key with `SETEX` and updates task status to `IN_PROGRESS`.
6. Service returns the task record. If no task is available, returns `204`.

## Completion flow

1. Worker submits result with `COMPLETED` or `FAILED`.
2. Service verifies task ownership and status.
3. Service persists artifacts (optional), stores the result record, updates task status, and clears the lease.
4. Service removes the task from the in-progress list.
5. Service posts webhook if the task contains a webhook URL.

## NACK flow

1. Worker submits `POST /tasks/:id/nack`.
2. Service verifies ownership and status.
3. Service computes backoff delay and moves the task to the delayed queue.
4. Service clears lease and removes the task from in-progress.

## Repair flows

- Claim-time repair: requeue expired leases during claim operations and move due delayed tasks to pending.

## Push notifications

codeQ emits two independent webhook classes:

- **Worker availability notifications**: Workers register a callback URL for event types. When new work becomes ready, codeQ sends a signal containing the event type and a recommended claim URL. The signal is advisory; the worker must still claim. Delivery can be `fanout`, `group` (one per group), or `hash` (deterministic selection) to balance notification load across worker fleets.

- **Result callbacks**: Producers can set a task-level webhook URL. When a task completes or fails, codeQ posts a result payload and retries with backoff. This replaces polling `GET /tasks/:id/result`.
