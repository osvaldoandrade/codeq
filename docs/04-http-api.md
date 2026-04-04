# HTTP API

All endpoints are under `/v1/codeq` except `/healthz` and `/metrics`.

## Common headers

- `Content-Type: application/json`
- `Authorization: Bearer <token>`
- `X-Request-Id` (optional): Correlation ID for request tracing. If not provided, the server generates a 16-byte hex random ID and includes it in the response header for log correlation.

## Tenant Isolation

All API operations are automatically scoped to the authenticated user's tenant. The tenant ID is extracted from JWT claims and used to isolate queue operations. Producers and workers can only access tasks within their own tenant.

## Create task

`POST /v1/codeq/tasks`

Auth: producer token.

**Rate limiting:** When producer rate limiting is enabled, may return `429 Too Many Requests` with:
- Header: `Retry-After: <seconds>` (time to wait before retry)
- Body: `{"error":"rate limit exceeded","scope":"producer","operation":"create_task","retryAfterSeconds":<N>}`

See [Rate Limiting](10-operations.md#rate-limiting) for configuration details.

Request body:

- `command` (string, required): queue command, e.g. `GENERATE_MASTER`
- `payload` (object, required)
- `priority` (int, optional, default 0)
- `maxAttempts` (int, optional, default 5)
- `webhook` (string, optional): result callback URL invoked on `COMPLETED` or `FAILED`
- `idempotencyKey` (string, optional): deduplication key with 24h TTL. Subsequent requests with same key return existing task. An in-process Bloom filter accelerates lookups for unique keys (see `docs/05-queueing-model.md#idempotency` for optimization details).
- `runAt` (string, optional): RFC3339 timestamp when the task becomes visible to workers. If `runAt` is in the past, the task is enqueued immediately.
- `delaySeconds` (int, optional): convenience alternative to `runAt` (relative to server time). If both are provided, `runAt` wins.

Example:

```json
{
  "command": "GENERATE_MASTER",
  "payload": {"jobId": "j-123"},
  "priority": 5,
  "runAt": "2026-01-25T13:10:00Z",
  "maxAttempts": 8,
  "webhook": "https://example.org/codeq/hook",
  "idempotencyKey": "job-j-123"
}
```

Response `202`:

````json
{
  "id": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
  "command": "GENERATE_MASTER",
  "payload": "{\"jobId\":\"j-123\"}",
  "priority": 5,
  "status": "PENDING",
  "tenantId": "tenant-abc123",
  "createdAt": "2026-01-25T10:00:00-03:00",
  "updatedAt": "2026-01-25T10:00:00-03:00"
}
````

The `tenantId` field is automatically populated from JWT claims and cannot be specified in the request. It ensures complete isolation between tenants.

## Create tasks (batch)

`POST /v1/codeq/tasks/batch`

Auth: producer token.

**Rate limiting:** When producer rate limiting is enabled, may return `429 Too Many Requests` (same as single create).

Batch create allows creating up to 100 tasks in a single request. Each task in the batch is processed independently—if one task fails, the others still succeed. The response contains results for each task.

Request body:

- `tasks` (array, required): array of task objects, max 100 items
  - Each task object has the same fields as the single create endpoint: `command`, `payload`, `priority`, `maxAttempts`, `webhook`, `idempotencyKey`, `runAt`, `delaySeconds`

Example:

```json
{
  "tasks": [
    {
      "command": "GENERATE_MASTER",
      "payload": {"jobId": "j-123"},
      "priority": 5
    },
    {
      "command": "RENDER_VIDEO",
      "payload": {"videoId": "v-456"},
      "priority": 3,
      "maxAttempts": 3
    }
  ]
}
```

Response `200`:

````json
{
  "results": [
    {
      "task": {
        "id": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
        "command": "GENERATE_MASTER",
        "payload": "{\"jobId\":\"j-123\"}",
        "priority": 5,
        "status": "PENDING",
        "tenantId": "tenant-abc123",
        "createdAt": "2026-01-25T10:00:00-03:00",
        "updatedAt": "2026-01-25T10:00:00-03:00"
      }
    },
    {
      "task": {
        "id": "b6b5d3be-6g8f-5b66-b181-39b0b5d3b9g5",
        "command": "RENDER_VIDEO",
        "payload": "{\"videoId\":\"v-456\"}",
        "priority": 3,
        "status": "PENDING",
        "tenantId": "tenant-abc123",
        "createdAt": "2026-01-25T10:00:00-03:00",
        "updatedAt": "2026-01-25T10:00:00-03:00"
      }
    }
  ]
}
````

**Notes:**
- Partial success is possible: each task result includes either `task` (success) or `error` (failure)
- Maximum batch size is 100 tasks
- All batch rate limiting is applied at the batch level (not per-task)
- `tenantId` is automatically populated for all created tasks

## Claim task (pull)

`POST /v1/codeq/tasks/claim`

Auth: worker token with `codeq:claim`.

**Rate limiting:** When worker claim rate limiting is enabled, may return `429 Too Many Requests` with:
- Header: `Retry-After: <seconds>` (time to wait before retry)
- Body: `{"error":"rate limit exceeded","scope":"worker","operation":"claim","retryAfterSeconds":<N>}`

See [Rate Limiting](10-operations.md#rate-limiting) for configuration details.

Request body:

- `commands` (array, optional): subset of token `eventTypes`. Default is token `eventTypes`.
- `leaseSeconds` (int, optional, default 300)
- `waitSeconds` (int, optional, default 0): long polling window, max 30.

Response:

- `200` with task
- `204` if no task is available

## Claim tasks (batch)

`POST /v1/codeq/tasks/claim/batch`

Auth: worker token with `codeq:claim`.

**Rate limiting:** When worker claim rate limiting is enabled, may return `429 Too Many Requests` (same as single claim).

Batch claim allows claiming up to 10 tasks in a single request. Tasks are claimed sequentially; if a claim succeeds but subsequent claims fail, partial results are returned.

Request body:

- `count` (int, required): number of tasks to claim, max 10
- `commands` (array, optional): subset of token `eventTypes`. Default is token `eventTypes`.
- `leaseSeconds` (int, optional, default 300)

Example:

```json
{
  "count": 5,
  "commands": ["GENERATE_MASTER", "RENDER_VIDEO"],
  "leaseSeconds": 600
}
```

Response `200` (when tasks are claimed):

````json
{
  "tasks": [
    {
      "id": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
      "command": "GENERATE_MASTER",
      "payload": "{\"jobId\":\"j-123\"}",
      "priority": 5,
      "status": "CLAIMED",
      "tenantId": "tenant-abc123",
      "claimedBy": "worker-123",
      "claimedAt": "2026-01-25T10:00:00-03:00",
      "expiresAt": "2026-01-25T10:10:00-03:00"
    },
    {
      "id": "b6b5d3be-6g8f-5b66-b181-39b0b5d3b9g5",
      "command": "RENDER_VIDEO",
      "payload": "{\"videoId\":\"v-456\"}",
      "priority": 3,
      "status": "CLAIMED",
      "tenantId": "tenant-abc123",
      "claimedBy": "worker-123",
      "claimedAt": "2026-01-25T10:00:05-03:00",
      "expiresAt": "2026-01-25T10:10:05-03:00"
    }
  ]
}
````

Response `204` (no content): when no tasks are available.

**Notes:**
- Maximum batch size is 10 tasks
- If some claims succeed and later claims fail, successful claims are returned with an `error` field indicating the reason for stopping
- Worker must have permission (`eventTypes`) for all claimed tasks
- Each claimed task has an `expiresAt` timestamp after which the lease expires and the task becomes visible again


## Heartbeat

`POST /v1/codeq/tasks/:id/heartbeat`

Auth: worker token with `codeq:heartbeat`.

Request body:

- `extendSeconds` (int, optional, default 300)

Response `200`:

```json
{"ok": true}
```

## Abandon

`POST /v1/codeq/tasks/:id/abandon`

Auth: worker token with `codeq:abandon`.

Response `200`:

```json
{"status": "requeued"}
```

## Submit result

`POST /v1/codeq/tasks/:id/result`

Auth: worker token with `codeq:result`.

Request body:

- `status` (string, required): `COMPLETED` or `FAILED`
- `result` (object, required if `COMPLETED`)
- `error` (string, required if `FAILED`)
- `artifacts` (array, optional)
  - `name` (string, required)
  - `url` (string, optional)
  - `contentBase64` (string, optional)
  - `contentType` (string, optional)

Response `200`:

```json
{
  "taskId": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
  "status": "COMPLETED",
  "result": {"ok": true},
  "artifacts": [{"name": "out.json", "url": "https://..."}],
  "completedAt": "2026-01-25T13:05:00Z"
}
```

Result submission is terminal. To retry work, use `/tasks/:id/nack` before completion.

When a task has `webhook`, codeQ sends a result callback with the payload defined in `docs/12-webhooks.md`.

## Submit results (batch)

`POST /v1/codeq/tasks/batch/results`

Auth: worker token with `codeq:result`.

Batch result submission allows submitting results for up to 100 tasks in a single request. Each result is processed independently—if one submission fails, the others still succeed.

Request body:

- `results` (array, required): array of result objects, max 100 items
  - `taskId` (string, required): the task ID
  - `status` (string, required): `COMPLETED` or `FAILED`
  - `result` (object, required if `COMPLETED`)
  - `error` (string, required if `FAILED`)
  - `artifacts` (array, optional): same structure as single submit result

Example:

```json
{
  "results": [
    {
      "taskId": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
      "status": "COMPLETED",
      "result": {"ok": true, "data": "..."}
    },
    {
      "taskId": "b6b5d3be-6g8f-5b66-b181-39b0b5d3b9g5",
      "status": "FAILED",
      "error": "Timeout during processing"
    }
  ]
}
```

Response `200`:

````json
{
  "results": [
    {
      "taskId": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
      "result": {
        "taskId": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
        "status": "COMPLETED",
        "result": {"ok": true, "data": "..."},
        "completedAt": "2026-01-25T13:05:00Z"
      }
    },
    {
      "taskId": "b6b5d3be-6g8f-5b66-b181-39b0b5d3b9g5",
      "result": {
        "taskId": "b6b5d3be-6g8f-5b66-b181-39b0b5d3b9g5",
        "status": "FAILED",
        "error": "Timeout during processing",
        "completedAt": "2026-01-25T13:05:10Z"
      }
    }
  ]
}
````

**Notes:**
- Partial success is possible: each result includes either a success response or an `error` field
- Maximum batch size is 100 results
- Result submission is terminal for each task—use single `/tasks/:id/nack` to retry
- Webhooks are triggered for each task that has one configured
- Worker ID is automatically populated from JWT claims for all results

## NACK

`POST /v1/codeq/tasks/:id/nack`

Auth: worker token with `codeq:nack`.

Request body:

- `delaySeconds` (int, optional): override backoff delay, capped by `backoffMaxSeconds`
- `reason` (string, optional)

Response `200`:

```json
{"status":"requeued","delaySeconds":30}
```

## Get task

`GET /v1/codeq/tasks/:id`

Auth: producer token or worker token.

## Get result

`GET /v1/codeq/tasks/:id/result`

Auth: producer token or worker token.

Response `200`:

```json
{"task": {"id": "..."}, "result": {"taskId": "..."}}
```

## Register webhook notifier (push)

`POST /v1/codeq/workers/subscriptions`

Auth: worker token with `codeq:subscribe`.

Request body:

- `callbackUrl` (string, required)
- `eventTypes` (array, optional): subset of token `eventTypes`
- `ttlSeconds` (int, optional, default 300)
- `deliveryMode` (string, optional): `fanout|group|hash`
- `groupId` (string, optional): required when `deliveryMode=group`. If token includes `workerGroup`, it must match.
- `minIntervalSeconds` (int, optional): rate limit per subscription, default 5

Response `200`:

```json
{"subscriptionId": "sub-123", "expiresAt": "2026-01-25T13:00:00Z"}
```

Defaulting:

- If token includes `workerGroup`, `deliveryMode` defaults to `group` and `groupId` defaults to `workerGroup`.
- If `deliveryMode` is omitted, it defaults to `fanout`.
- If `deliveryMode=group` and `groupId` cannot be resolved, return `400`.

## Renew webhook notifier

`POST /v1/codeq/workers/subscriptions/:id/heartbeat`

Auth: worker token.

## Admin queues

`GET /v1/codeq/admin/queues`

Auth: admin token.

## Admin queue stats (per event)

`GET /v1/codeq/admin/queues/:command`

Auth: admin token.

Response `200`:

```json
{
  "command": "RENDER_VIDEO",
  "ready": 124,
  "delayed": 5,
  "inProgress": 5,
  "dlq": 0
}
```

## Admin cleanup

`POST /v1/codeq/admin/tasks/cleanup`

Auth: admin token.

**Rate limiting:** When admin cleanup rate limiting is enabled, may return `429 Too Many Requests` with:
- Header: `Retry-After: <seconds>` (time to wait before retry)
- Body: `{"error":"rate limit exceeded","scope":"admin","operation":"cleanup","retryAfterSeconds":<N>}`

See [Rate Limiting](10-operations.md#rate-limiting) for configuration details.

## Observability

### Prometheus Metrics

`GET /metrics`

Returns Prometheus-formatted metrics for monitoring and observability.

**Authentication:** None (public endpoint)

**Response Format:** `text/plain; version=0.0.4`

**Metrics Exposed:**

- **HTTP metrics**: Request counts, duration, response sizes per endpoint
- **Queue metrics**: 
  - Task counts by state (queued, claimed, completed, failed, DLQ)
  - Queue depth by command type
  - Claim success/failure rates
- **Redis metrics**:
  - Connection pool stats
  - Command latencies
  - Memory usage
- **Go runtime metrics**: Goroutines, memory, GC stats

**Example:**

````bash
curl http://localhost:8080/metrics
````

**Sample Response:**

````
# HELP codeq_tasks_created_total Total number of tasks created
# TYPE codeq_tasks_created_total counter
codeq_tasks_created_total{command="GENERATE_MASTER"} 1523

# HELP codeq_tasks_claimed_total Total number of tasks claimed
# TYPE codeq_tasks_claimed_total counter
codeq_tasks_claimed_total{command="GENERATE_MASTER"} 1520

# HELP codeq_queue_depth Current number of tasks in queue
# TYPE codeq_queue_depth gauge
codeq_queue_depth{command="GENERATE_MASTER",state="queued"} 12
````

**Usage:**

Configure Prometheus to scrape this endpoint:

````yaml
scrape_configs:
  - job_name: 'codeq'
    static_configs:
      - targets: ['codeq:8080']
    metrics_path: '/metrics'
    scrape_interval: 15s
````

See [Operations Guide](10-operations.md#monitoring) for full metrics reference and Grafana dashboard setup.
