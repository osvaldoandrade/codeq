# Worker Availability Webhook

This flow uses webhook notifications to reduce idle worker polling.

Notifications are advisory. They do not assign tasks and they do not change task ownership. A worker must still claim via `POST /v1/codeq/tasks/claim`.

## Preconditions

- Worker has a JWT with `codeq:subscribe`.
- Worker provides a reachable `callbackUrl`.

## Main flow

1. Worker registers a subscription using `POST /v1/codeq/workers/subscriptions`.
2. codeQ stores the subscription with TTL and requires renewal.
3. When work becomes available for an event type (queue transitions non-empty, delayed becomes due, requeue makes ready), codeQ sends a signed webhook notification.
4. Worker validates signature and timestamp.
5. Worker calls `POST /v1/codeq/tasks/claim` to obtain ownership.

## Sequence diagram

```mermaid
sequenceDiagram
  participant W as Worker
  participant Q as codeQ
  participant K as KVRocks
  participant CB as Worker Callback URL

  W->>Q: POST /workers/subscriptions
  Q->>K: Persist subscription with TTL
  Q-->>W: 200 OK (subscriptionId)

  Note over Q: Work becomes ready for eventType
  Q->>CB: POST signed notification
  CB-->>Q: 200 OK

  W->>Q: POST /tasks/claim
  Q->>K: Claim + lease
  Q-->>W: 200 OK (Task)
```
