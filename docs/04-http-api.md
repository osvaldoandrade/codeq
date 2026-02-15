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

## Health

`GET /healthz`
