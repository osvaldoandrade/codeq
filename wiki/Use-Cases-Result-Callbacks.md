# Use Case: Result Callbacks

This flow uses task-level webhooks to avoid polling `GET /tasks/:id/result`.

## Preconditions

- Producer creates the task with a `webhook` URL.
- Producer can validate callback signatures.

## Main flow

1. Producer calls `POST /v1/codeq/tasks` with the `webhook` field.
2. codeQ stores the webhook URL in the task record.
3. Worker claims and completes the task.
4. When the task reaches `COMPLETED` or `FAILED`, codeQ POSTs a signed callback with the result payload.
5. If delivery fails, codeQ retries with exponential backoff (bounded by configuration).
6. Producer deduplicates callbacks by `taskId` (at-least-once delivery).

## Sequence diagram

```mermaid
sequenceDiagram
  participant P as Producer
  participant Q as codeQ
  participant W as Worker
  participant CB as Producer Callback URL

  P->>Q: POST /tasks (webhook=CB)
  Q-->>P: 202 Accepted (taskId)

  W->>Q: POST /tasks/claim
  Q-->>W: 200 OK (Task)
  W->>Q: POST /tasks/:id/result
  Q-->>W: 200 OK

  Q->>CB: POST signed result callback
  alt callback fails
    Q->>CB: Retry with backoff
  end
```
