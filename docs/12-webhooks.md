# Webhooks and push delivery

This document defines two independent webhook paths:

1. **Worker availability notifications** (push mode for workers)
2. **Result callbacks** (push mode for producers to avoid GET polling)

## 1) Worker availability notifications

Webhook notifications are advisory signals that work is available. They do not assign tasks and do not change task ownership. Workers must still claim tasks using the pull API.

### Registration

Workers register a callback URL with an event type list. Registrations are stored with TTL and must be renewed. Registration is ephemeral and does not create a worker registry.

Recommended fields for subscriptions:

- `callbackUrl` (string, required)
- `eventTypes` (array, optional): subset of token `eventTypes`
- `ttlSeconds` (int, optional): default 300
- `deliveryMode` (string, optional): `fanout|group|hash`
- `groupId` (string, optional): required when `deliveryMode=group`. If the worker token includes `workerGroup`, the request `groupId` must match it.
- `minIntervalSeconds` (int, optional): rate limit per subscription, default 5

### Delivery modes

### fanout

Notify every active subscription that matches the event type. This maximizes wake-ups and is suitable for small worker fleets.

### group

Notify exactly one subscriber per `groupId` and event type. Selection uses round-robin over active subscribers in that group. This avoids thundering herd when multiple worker instances belong to the same service pool. If `workerGroup` is present in the token, it is the authoritative group id.

### hash

Notify a deterministic subscriber chosen by a time-bucketed index over the active subscription list. This provides simple routing without stateful round-robin, at the cost of uneven distribution when the subscriber set changes.

## Defaulting rules

- If the worker token includes `workerGroup`, `deliveryMode` defaults to `group` and `groupId` defaults to `workerGroup`.
- If `deliveryMode` is omitted, it defaults to `fanout`.
- If `deliveryMode=group` and `groupId` cannot be resolved, the request is rejected with `400`.

### Notification payload

```json
{
  "eventType": "generate_master",
  "available": true,
  "queueDepth": 42,
  "claimUrl": "/v1/codeq/tasks/claim",
  "sentAt": "2026-01-25T13:00:00Z",
  "notificationId": "ntf-2c6c3b2a"
}
```

`queueDepth` is advisory. Workers must rely on claim response for correctness.

### Trigger conditions

Notifications are sent when:

- a task transitions to ready in an empty queue
- a delayed task becomes ready
- a requeued task makes the queue non-empty

Notifications are best-effort and not retried by default.

### Multiple worker instances

Recommended patterns:

1. Load-balanced callback URL with `deliveryMode=fanout`. The worker fleet self-balances.
2. Per-instance subscriptions with the same `groupId` and `deliveryMode=group` for one wake-up per pool.

Because the notification is advisory, duplicate signals do not affect correctness. Use `minIntervalSeconds` to reduce bursty notifications.

### Security

Notifications include:

- `X-CodeQ-Timestamp`: Unix epoch seconds
- `X-CodeQ-Signature`: HMAC-SHA256 over `timestamp + '.' + body`

Workers must reject stale timestamps and invalid signatures.

When OpenTelemetry tracing is enabled, notifications also include W3C trace context headers:

- `traceparent`
- `tracestate` (optional)

## 2) Result callbacks

Result callbacks are used to avoid polling `GET /tasks/:id/result`. They are triggered when a task reaches a terminal state (`COMPLETED` or `FAILED`).

### Registration

Result callbacks are configured per task at creation time using the `webhook` field. The callback URL is stored in the task record. For applications that require streaming updates, a WebSocket or SSE gateway can consume these callbacks and forward them to clients.

### Payload

```json
{
  "taskId": "a5a4d2ad-5f7e-4a55-a070-29a9a4c2a8f4",
  "eventType": "render_video",
  "status": "COMPLETED",
  "result": {"ok": true},
  "error": "",
  "artifacts": [{"name":"out.json","url":"https://..."}],
  "completedAt": "2026-01-25T13:05:00Z"
}
```

### Delivery guarantees

Callbacks are best-effort and at-least-once. Consumers must de-duplicate by `taskId`.

### Retry policy

If the callback fails, codeQ retries using an exponential backoff:

- `resultWebhookMaxAttempts` (default 5)
- `resultWebhookBaseBackoffSeconds` (default 2)
- `resultWebhookMaxBackoffSeconds` (default 60)

### Security

Result callbacks include the same signature headers used by worker notifications:

- `X-CodeQ-Timestamp`
- `X-CodeQ-Signature` (HMAC-SHA256 over `timestamp + '.' + body`)

Producers must reject stale timestamps and invalid signatures.

When OpenTelemetry tracing is enabled, result callbacks also include W3C trace context headers:

- `traceparent`
- `tracestate` (optional)
