# IO REST API

The REST API is codeq's language-agnostic surface. It is a conventional Gin HTTP/1.1 service on port `:8080`, with routes registered in `pkg/app/url_mappings.go` and handlers in `internal/controllers/`. Every operation that exists on the gRPC streams has a REST equivalent, and every operation reachable from a `curl` command line is reachable through the REST API. The trade-off is throughput: each REST call pays for Gin's router, the authentication middleware, the rate limiter, JSON marshalling on both sides, and the standard library's response-writer mutex. A single client connection tops out at roughly 3,000 to 4,000 requests per second on this path because of `net/http`'s internal write serialization. For higher throughput see [IO-Producer-Stream](IO-Producer-Stream) and [IO-Worker-Stream](IO-Worker-Stream).

The point of keeping the REST API is operational. A developer debugging an issue can curl an endpoint and inspect the JSON. A non-Go client (a shell script, a Python notebook, a one-off integration) can talk to codeq without a gRPC code generator. A monitoring system can poll task state or raft status without a stream. And a producer that creates a handful of tasks per minute does not need the complexity of a long-lived gRPC connection; one POST per task is enough. The REST API is the path of least surprise for everyone whose throughput needs sit below the gRPC threshold.

## Endpoint table

The full set of REST endpoints under `/v1/codeq` is short and covers the same operations as the gRPC streams, plus the read paths that gRPC does not handle.

| Method | Path                                  | Purpose                                                                   |
|--------|---------------------------------------|---------------------------------------------------------------------------|
| POST   | `/v1/codeq/tasks`                     | Create one task. Body mirrors `CreateRequest`.                            |
| POST   | `/v1/codeq/tasks/batch`               | Create N tasks in one request. Body is an array.                          |
| POST   | `/v1/codeq/tasks/claim`               | Claim one available task. Body specifies commands and lease.              |
| POST   | `/v1/codeq/tasks/claim/batch`         | Claim up to N tasks in one request.                                       |
| POST   | `/v1/codeq/tasks/<id>/result`         | Submit the result for a claimed task. Body is `{status, result, error}`.  |
| POST   | `/v1/codeq/tasks/<id>/heartbeat`      | Extend the lease on a claimed task.                                       |
| POST   | `/v1/codeq/tasks/<id>/nack`           | Return a claimed task to the queue with a delay and a reason.             |
| POST   | `/v1/codeq/tasks/<id>/abandon`        | Release the lease on a claimed task without nacking.                      |
| POST   | `/v1/codeq/tasks/batch/results`       | Submit results for N claimed tasks in one request.                        |
| GET    | `/v1/codeq/tasks/<id>`                | Fetch a task's current state.                                             |
| GET    | `/v1/codeq/tasks/<id>/result`         | Fetch the result of a completed task.                                     |
| GET    | `/v1/codeq/raft/status`               | Local-node view of per-shard raft leadership.                             |
| POST   | `/v1/codeq/workers/subscriptions`     | Register a long-poll subscription for a command.                          |
| POST   | `/v1/codeq/workers/subscriptions/<id>/heartbeat` | Keep a subscription alive.                                     |
| GET    | `/v1/codeq/admin/queues`              | Admin: list known commands and their queue depths.                        |
| GET    | `/v1/codeq/admin/queues/<command>`    | Admin: stats for one command's queue.                                     |
| POST   | `/v1/codeq/admin/tasks/cleanup`       | Admin: drop expired entries from the delay index.                         |
| GET    | `/metrics`                            | Prometheus scrape endpoint (no auth, by design).                          |

The producer endpoints are gated by `middleware.AuthMiddleware`, the worker endpoints by `middleware.WorkerAuthMiddleware`, and the read endpoints (`GET /tasks/<id>`, `GET /raft/status`) by `middleware.AnyAuthMiddleware`, which accepts either token. Admin endpoints additionally require `middleware.RequireAdmin`. Route registration sits at `pkg/app/url_mappings.go:11`.

## Authentication

Every endpoint except `/metrics` requires a bearer token in the `Authorization` header. The header format is the standard `Authorization: Bearer <token>` shape, parsed by `validateBearer` in `internal/middleware/auth.go:34`. The string is split on whitespace; the first part is matched case-insensitively against `Bearer`, the second part is passed to the configured `auth.Validator`. A missing header, a malformed header, or an invalid token returns `401 Unauthorized` with a JSON body containing the error.

The validator is constructed at app startup from the configuration. Producer and worker tokens come from separate JWT keys by default, which lets operators rotate them independently and lets the worker server identify which audience a token belongs to. A token's `eventTypes` claim controls which commands the worker can claim, and the `scopes` claim controls which operations the token permits (`codeq:claim`, `codeq:result`, `codeq:heartbeat`, `codeq:nack`, `codeq:abandon`, `codeq:subscribe`). The middleware checks scopes per-route via `middleware.RequireWorkerScope`, so a token granted only `codeq:claim` cannot submit results.

The same Authorization header works on the gRPC streams, but indirectly. The token is passed as the `Hello.token` field of the first event, not as an HTTP header, because gRPC bidirectional streams have only one HTTP request (the stream open) and the token must live on the application message. The validation logic and the claim shape are identical across REST and gRPC, so an operator who issues a token can use it on either surface.

## Creating tasks

The simplest path is one POST per task. The body is JSON; the fields mirror the gRPC `CreateRequest` documented in [IO-Producer-Stream](IO-Producer-Stream).

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H "Authorization: Bearer $PRODUCER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "command": "send-email",
    "payload": {"to": "a@b.com"},
    "priority": 10,
    "maxAttempts": 3
  }'
```

The server returns `201 Created` with the task ID and the assigned tenant on success, or `400 Bad Request` with a JSON error body on validation failure. The handler is `controllers.NewCreateTaskController` (`internal/controllers/create_task_controller.go`). It does the minimum work the gRPC path also does — validate the command, parse the payload, call `SchedulerService.CreateTask` — and serializes the resulting `domain.Task` back as JSON.

For higher throughput, the batch endpoint at `POST /v1/codeq/tasks/batch` accepts an array of CreateRequest bodies and returns an array of results. The handler is `controllers.NewBatchCreateTaskController`. This is the REST equivalent of the gRPC `ProduceBatch` call and pays one HTTP round trip and one Pebble batch commit for N tasks. It is the right shape when the producer already has a batch of work in hand and wants the latency win of a single round trip without adopting the stream.

The `idempotencyKey` field, set in the body, is the standard way to make CreateTask retry-safe. CodeQ deduplicates against the key for a configurable window; a second POST with the same key returns the original task's ID rather than creating a duplicate. Applications that retry CreateTask on network failures should always set the key — otherwise a successful create followed by a failed response can produce two tasks.

## Claiming and completing tasks

The worker REST path is symmetric. A worker POSTs `/v1/codeq/tasks/claim` with a body specifying the commands it can handle and the lease duration. The server returns either `200 OK` with a task body or `204 No Content` if the queue is empty. The handler is `controllers.NewClaimTaskController` (`internal/controllers/claim_task_controller.go`).

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H "Authorization: Bearer $WORKER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "commands": ["send-email"],
    "leaseSeconds": 60,
    "workerId": "worker-1"
  }'
```

On success the response body is a `domain.Task` carrying the same fields the gRPC stream surfaces — id, command, payload, priority, attempts, maxAttempts, tenantId, leaseUntil. The worker then runs its handler and POSTs the result back.

```bash
curl -X POST "http://localhost:8080/v1/codeq/tasks/${TASK_ID}/result" \
  -H "Authorization: Bearer $WORKER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "status": "COMPLETED",
    "workerId": "worker-1",
    "result": {"sentAt": "2026-05-18T12:00:00Z"}
  }'
```

The `status` field must be `COMPLETED` or `FAILED`. The handler is `controllers.NewSubmitResultController` (`internal/controllers/submit_result_controller.go`). Heartbeat, nack, and abandon are the same shape — a POST to the per-task endpoint with the necessary fields in the body. Heartbeat takes `extendSeconds`, Nack takes `delaySeconds` and `reason`, Abandon takes no body. The handlers live in `internal/controllers/heartbeat_controller.go`, `nack_task_controller.go`, and `abandon_task_controller.go` respectively.

The batch-claim and batch-result endpoints are the natural pair for a worker that wants the REST surface but the throughput of grouping. They mirror the gRPC TaskBatch and ResultBatch wire shapes: one round trip claims up to N tasks, one round trip submits up to N results. Workers that cannot adopt the gRPC stream but need throughput above the per-request ceiling should batch.

## Reading task state

The two GET endpoints under `/v1/codeq/tasks/<id>` cover the read paths. `GET /v1/codeq/tasks/<id>` returns the current `domain.Task` — status, attempts, lease, worker assignment. The handler is `controllers.NewGetTaskController` (`internal/controllers/get_task_controller.go`). `GET /v1/codeq/tasks/<id>/result` returns the result payload that the worker recorded on Completed, or the error string on Failed. The handler is `controllers.NewGetResultController` (`internal/controllers/get_result_controller.go`). Both endpoints accept either token via `AnyAuthMiddleware`, because reading task state is a legitimate operation for both the producer that created the task and the worker that processed it.

The result endpoint is the path applications use to implement request/response semantics on top of an asynchronous queue. A producer that wants the worker's result polls `GET /v1/codeq/tasks/<id>/result` until status flips from PENDING to COMPLETED. For higher-latency cases the producer can supply a `webhook` URL in the original CreateRequest, in which case codeq POSTs the result to the webhook when it lands instead of requiring the producer to poll.

The raft status endpoint at `GET /v1/codeq/raft/status` returns the local node's view of every shard's raft group. The body lists each shard's current leader, voters, and bind address. It is the standard probe for operational dashboards and is one of the few endpoints that is intentionally public-via-token rather than admin-gated, because Prometheus scrapers and other monitoring tools need access without an admin role. See [IO-Raft-Replication](IO-Raft-Replication) for the underlying group structure and [Observability-Metrics](Observability-Metrics) for the scrape conventions.

## When to choose REST over gRPC

The decision is rarely close. REST is the right choice for one-off scripts, for non-Go clients, for ad-hoc operational queries, and for debugging. The gRPC streams are the right choice for any sustained workload — anything that creates more than a few hundred tasks per second or claims more than a handful of tasks per second over a long period.

The REST API's throughput ceiling has three causes. The first is `net/http`'s response-writer mutex, which serializes writes to the connection's buffered writer; this caps single-connection POST throughput at the low thousands. The second is the per-request middleware cost: JWT validation, rate-limiter consultation, JSON encoding and decoding. The third is the per-request allocation cost: Gin allocates a context object per request, a request body buffer, and a response body buffer. The gRPC streams pay each of those costs once per session rather than per operation.

The REST API's other limit is statelessness. Each REST request is independent, so it cannot ride on session-scoped optimisations like the gRPC stream's amortised authentication. Workers that need fast claim cycles benefit enormously from session-scoped state (one Hello, many Ready), and a REST worker pays the full middleware stack on every claim.

That said, there are workloads where REST is the better fit even at scale. A producer that runs many short-lived processes, each creating one task and exiting, has no opportunity to amortise a gRPC session and would pay the Hello round-trip per task. A worker fleet with hundreds of small services, each claiming one task per minute, benefits from REST's simplicity more than from the stream's throughput. The default advice is "use the gRPC SDK if you can; use REST if you cannot or need not".

## Error responses

Every REST endpoint returns errors as JSON with a consistent shape. The body carries a `code` field (a stable string like `not-found`, `validation-error`, `permission-denied`) and a `message` field (a human-readable description). The HTTP status code is the standard one for the situation: `400` for client validation errors, `401` for missing or invalid auth, `403` for permission errors (worker tries to submit a result for a task it does not own), `404` for missing tasks, `409` for idempotency conflicts, `500` for server errors. The shape is defined in `internal/controllers/respond.go` and used uniformly across all handlers.

The 307 redirect is the one non-standard response that is worth knowing. When the local codeq node is not the raft leader for the task's shard, the handler returns `307 Temporary Redirect` with the `Location` header pointing at the leader's REST endpoint. Clients are expected to follow the redirect; `curl -L` does this automatically, the Go SDK does it inside its HTTP transport, and other clients should handle 307 like any other redirect. See [IO-Raft-Replication](IO-Raft-Replication) for the routing protocol.

## Rate limiting

The producer and worker rate limiters live on `middleware.RateLimitProducer` and `middleware.RateLimitWorkerClaim`. Each is a token-bucket limiter keyed on the bearer token's subject. The bucket sizes and refill rates are configurable via `RateLimit.Producer` and `RateLimit.Worker` in the YAML config. Limited requests return `429 Too Many Requests` with a `Retry-After` header indicating how many seconds to wait before retrying. The limiters apply only to the REST surface; the gRPC streams enforce rate limits per session at Hello time, not per event, because the per-event cost on the stream is already small enough that token-bucket limiting would be both expensive and unnecessary. See [Performance-Tuning](Performance-Tuning) for limiter sizing guidance.

## Versioning

Every endpoint sits under `/v1/codeq`. The `v1` prefix is the stable contract: codeq will not break backward compatibility under `v1`. Additive changes (new fields on response bodies, new optional fields on request bodies) are not breaking and may appear in minor releases. Breaking changes will move to `/v2/codeq` and run alongside `v1` for at least one release cycle. The current API surface (the table above) is the entirety of `v1` as of the documented release; the gRPC contracts in `producerpb` and `workerpb` carry a separate versioning story documented under [Concepts-Wire-Format](Concepts-Wire-Format).
