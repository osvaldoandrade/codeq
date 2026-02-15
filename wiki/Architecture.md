# Architecture

codeQ is implemented as a stateless HTTP API backed by a stateful KVRocks keyspace.

The service has no worker registry and no background "scheduler" that assigns tasks. Instead, the scheduler logic is embedded in claim and completion operations: claim-time repair moves due delayed tasks back to ready, detects expired or missing leases, and requeues work when needed.

## Component View

```mermaid
graph TD
  P[Producer] -->|POST /tasks| API[codeQ HTTP API]
  W[Worker] -->|POST /tasks/claim| API
  W -->|POST /tasks/:id/result| API
  API --> CORE[Scheduler core]
  CORE --> KV[(KVRocks)]
  API --> NT[Notifier]
  NT -->|webhook signals| WH[Worker Callback URL]
  API --> ART[Artifact storage]
```

## Enqueue Flow

A producer creates a task by providing:

- `command` (event type)
- `payload`
- optional `priority`, `maxAttempts`, `idempotencyKey`
- optional task-level result callback `webhook`

The service validates and normalizes the payload, persists the task record, and inserts the task ID into the ready queue for that command.

```mermaid
sequenceDiagram
  participant P as Producer
  participant A as codeQ API
  participant K as KVRocks

  P->>A: POST /v1/codeq/tasks
  A->>A: Validate + normalize payload
  A->>K: HSET codeq:tasks[id] = Task
  A->>K: LPUSH codeq:q:<cmd>:pending:<prio> id
  A->>K: ZADD codeq:tasks:ttl id retentionCutoff
  A-->>P: 202 Accepted (Task)
```

## Claim Flow (Pull)

A worker claims tasks by command. Claim is intentionally narrow: "give me one task for one of these commands".

Claim includes a repair loop:

- move due delayed tasks back to ready
- requeue tasks that are in-progress but have missing/expired leases

Then it atomically pops one ID from pending and tracks it in in-progress via Lua (`RPOP` + `SADD`), sets a lease key with TTL, and updates the task record.

```mermaid
sequenceDiagram
  participant W as Worker
  participant A as codeQ API
  participant K as KVRocks

  W->>A: POST /v1/codeq/tasks/claim
  A->>A: Validate worker token + filter commands
  A->>A: Claim-time repair (due delayed + expired leases)
  A->>K: EVAL claim move (RPOP pending + SADD inprog)
  A->>K: SETEX codeq:lease:<id> leaseSeconds workerId
  A->>K: HSET codeq:tasks[id] status=IN_PROGRESS, workerId, leaseUntil
  A-->>W: 200 OK (Task) OR 204 No Content
```

## Completion Flow

Completion is terminal: once a task is `COMPLETED` or `FAILED`, it is not automatically retried.

The service persists the result record, clears the lease, removes the task from in-progress, and optionally triggers a task-level result callback webhook.

```mermaid
sequenceDiagram
  participant W as Worker
  participant A as codeQ API
  participant K as KVRocks
  participant H as Result Webhook URL

  W->>A: POST /v1/codeq/tasks/:id/result
  A->>A: Verify ownership + status
  A->>K: HSET codeq:results[id] = Result
  A->>K: DEL codeq:lease:<id>
  A->>K: SREM codeq:q:<cmd>:inprog id
  A->>K: HSET codeq:tasks[id] status=COMPLETED/FAILED
  alt task has webhook
    A->>H: POST signed callback (best-effort + retry)
  end
  A-->>W: 200 OK (Result)
```

## NACK + Retry

A nack transitions a task back into the delayed queue and clears ownership. The service computes the delay using the configured backoff policy (or a capped override).

```mermaid
sequenceDiagram
  participant W as Worker
  participant A as codeQ API
  participant K as KVRocks

  W->>A: POST /v1/codeq/tasks/:id/nack
  A->>A: Verify ownership + status
  A->>A: attempts++ and compute delaySeconds
  A->>K: ZADD codeq:q:<cmd>:delayed visibleAt id
  A->>K: DEL codeq:lease:<id>
  A->>K: SREM codeq:q:<cmd>:inprog id
  A->>K: HSET codeq:tasks[id] status=PENDING, workerId=""
  A-->>W: 200 OK (delaySeconds)
```

## Push Without Assignment

codeQ implements two push paths:

- worker availability notifications: advisory signals that work is available for an event type
- result callbacks: task-level completion hooks

Both are described in [Webhooks](Webhooks).
