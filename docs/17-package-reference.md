# Package Reference

This document provides detailed information about the codeQ codebase structure and package responsibilities.

## Repository Layout

````
codeq/
├── cmd/codeq/          # CLI entrypoint
├── pkg/                # Public API packages
│   ├── app/            # Application bootstrap
│   ├── config/         # Configuration
│   └── domain/         # Domain entities
├── internal/           # Private implementation
│   ├── backoff/        # Retry logic
│   ├── controllers/    # HTTP handlers
│   ├── middleware/     # Auth & middleware
│   ├── providers/      # External services
│   ├── repository/     # Data access
│   └── services/       # Business logic
├── helm/codeq/         # Kubernetes Helm chart
├── docs/               # Documentation
├── test/               # Integration tests
└── wiki/               # GitHub Pages content
````

## Public Packages (`pkg/`)

### `pkg/app`

**Purpose**: Application initialization and HTTP server setup

**Key files**:
- `application.go`: Main `Application` struct with `Start()` method
- `url_mappings.go`: HTTP route definitions (Gin router)
- `integration_test.go`: End-to-end integration tests

**Usage**: Imported by service runtime (see `github.com/codecompany/codeq-service`)

**Example**:
````go
import "github.com/osvaldoandrade/codeq/pkg/app"

app := app.NewApplication(config)
app.Start() // Starts HTTP server on configured port
````

---

### `pkg/config`

**Purpose**: Configuration loading, validation, and defaults

**Key files**:
- `config.go`: `Config` struct with YAML unmarshaling and environment variable overrides

**Features**:
- Loads from YAML file
- Overrides via environment variables (e.g., `PORT`, `REDIS_ADDR`)
- Auto-computed defaults (e.g., `identityJwksUrl` from `identityServiceUrl`)
- Validation warnings for missing required fields

**Example**:
````go
import "github.com/osvaldoandrade/codeq/pkg/config"

cfg, err := config.LoadConfig("config.yaml")
if err != nil {
    log.Fatal(err)
}
// cfg.Port, cfg.RedisAddr, etc. are now available
````

**See**: `docs/14-configuration.md` for full configuration reference

---

### `pkg/domain`

**Purpose**: Core domain types (entities, value objects)

**Key files**:
- `task.go`: `Task`, `Command` types
- `result.go`: `Result`, `Artifact`, `SubmitResultRequest` types
- `subscription.go`: `Subscription` type (webhook subscriptions)
- `queue_stats.go`: `QueueStats` type (admin metrics)

**Design**:
- No dependencies on other packages (pure domain layer)
- Types are serialized to/from JSON and stored in Redis
- All timestamps use `time.Time` and are serialized as RFC3339

**Example**:
````go
import "github.com/osvaldoandrade/codeq/pkg/domain"

task := &domain.Task{
    ID:       "abc-123",
    Command:  "GENERATE_MASTER",
    Payload:  `{"jobId":"j-1"}`,
    Priority: 5,
    Status:   "PENDING",
}
````

**See**: `docs/02-domain-model.md` for entity definitions

---

## Internal Packages (`internal/`)

### `internal/controllers`

**Purpose**: HTTP request handlers (Gin framework)

**Key files**:
- **Task lifecycle**:
  - `create_task_controller.go`: `POST /tasks` (producer auth)
  - `claim_task_controller.go`: `POST /tasks/claim` (worker auth)
  - `submit_result_controller.go`: `POST /tasks/:id/result` (worker auth)
  - `nack_task_controller.go`: `POST /tasks/:id/nack` (worker auth)
  - `abandon_task_controller.go`: `POST /tasks/:id/abandon` (worker auth)
  - `heartbeat_controller.go`: `POST /tasks/:id/heartbeat` (worker auth)
  - `get_task_controller.go`: `GET /tasks/:id` (any auth)
  - `get_result_controller.go`: `GET /tasks/:id/result` (any auth)

- **Webhook subscriptions**:
  - `create_subscription_controller.go`: `POST /workers/subscriptions` (worker auth)
  - `heartbeat_subscription_controller.go`: `POST /workers/subscriptions/:id/heartbeat` (worker auth)

- **Admin operations**:
  - `queue_admin_controller.go`: `GET /admin/queues` (admin auth)
  - `queue_stats_controller.go`: `GET /admin/queues/:command` (admin auth)
  - `cleanup_expired_controller.go`: `POST /admin/tasks/cleanup` (admin auth)

**Pattern**: Controllers bind JSON, validate input, call services, return JSON response

**Example**:
````go
func CreateTaskController(svc *services.SchedulerService) gin.HandlerFunc {
    return func(c *gin.Context) {
        var req CreateTaskRequest
        if err := c.ShouldBindJSON(&req); err != nil {
            c.JSON(400, gin.H{"error": err.Error()})
            return
        }
        task, err := svc.CreateTask(c.Request.Context(), req)
        if err != nil {
            c.JSON(500, gin.H{"error": err.Error()})
            return
        }
        c.JSON(202, task)
    }
}
````

---

### `internal/middleware`

**Purpose**: Request preprocessing (auth, logging, correlation IDs)

**Key files**:
- **Authentication**:
  - `auth.go`: Producer token validation (Tikti/Identity JWKS)
  - `worker_auth.go`: Worker JWT validation (JWKS)
  - `any_auth.go`: Accept either producer or worker token
  - `require_admin.go`: Admin scope validation
  - `worker_scope.go`: Filter event types by token claims

- **Request processing**:
  - `logger.go`: Request/response logging
  - `request_id.go`: Inject `X-Request-Id` for correlation

**Auth flow**:
1. Middleware extracts `Authorization: Bearer <token>` header
2. Validates JWT signature via JWKS
3. Checks `iss`, `aud`, `exp`, `nbf` claims
4. Extracts identity and scopes (e.g., `codeq:claim`, `eventTypes`)
5. Stores identity in Gin context for controllers

**Example**:
````go
router.POST("/tasks/claim",
    middleware.WorkerAuth(config),
    middleware.WorkerScope("codeq:claim"),
    controllers.ClaimTaskController(svc),
)
````

**See**: `docs/09-security.md` for authentication details

---

### `internal/services`

**Purpose**: Business logic layer (orchestration, state machines, webhooks)

**Key files**:
- `scheduler_service.go`: Core scheduling logic
  - `CreateTask()`: Enqueue task, handle delayed scheduling
  - `ClaimTask()`: Pull task from queue, run repair, assign lease
  - `NackTask()`: Compute backoff, requeue to delayed queue
  - `AbandonTask()`: Requeue to ready queue
  - `HeartbeatTask()`: Extend lease
  - `RepairExpiredLeases()`: Requeue tasks with expired leases (claim-time repair)

- `results_service.go`: Result storage and validation
  - `SubmitResult()`: Store result, upload artifacts, clear lease
  - `GetResult()`: Fetch result by task ID

- `result_callback_service.go`: Task-level webhook delivery
  - `SendResultCallback()`: POST result to task webhook URL with retry

- `subscription_service.go`: Worker availability subscription management
  - `CreateSubscription()`: Register webhook listener
  - `HeartbeatSubscription()`: Extend subscription TTL

- `notifier_service.go`: Worker availability notification dispatch
  - `NotifyWorkers()`: Find subscriptions for command, dispatch webhooks

- `subscription_cleanup_service.go`: Background cleanup
  - `CleanupExpired()`: Remove expired subscriptions

**Service dependencies**: Services call repositories, providers, and other services

**Example**:
````go
task, err := schedulerSvc.CreateTask(ctx, CreateTaskRequest{
    Command:  "GENERATE_MASTER",
    Payload:  map[string]any{"jobId": "j-1"},
    Priority: 5,
})
````

---

### `internal/repository`

**Purpose**: Data access layer (Redis operations, queue semantics)

**Key files**:
- `task_repository.go`: Task CRUD and queue operations
  - `CreateTask()`, `GetTask()`, `UpdateTask()`
  - `PushReady()`: Add to ready queue (LPUSH)
  - `PopReady()`: Claim from ready queue (RPOPLPUSH)
  - `PushDelayed()`: Schedule for future (ZADD with score = runAt)
  - `MoveDelayedToReady()`: Requeue due tasks (ZRANGEBYSCORE + LPUSH)
  - `GetInProgressExpired()`: Find expired leases (ZRANGEBYSCORE)
  - `GetQueueStats()`: Count tasks by status (LLEN, ZCARD)

- `result_repository.go`: Result storage
  - `SaveResult()`, `GetResult()`

- `subscription_repository.go`: Subscription storage
  - `CreateSubscription()`, `GetSubscription()`, `RenewSubscription()`, `DeleteSubscription()`
  - `FindSubscriptionsByEventType()`: Query for webhook dispatch

**Redis layout**: See `docs/07-storage-kvrocks.md`

**Example**:
````go
// Ready queue operations
taskRepo.PushReady(ctx, "GENERATE_MASTER", taskID)
taskID, err := taskRepo.PopReady(ctx, "GENERATE_MASTER", workerID)

// Delayed queue operations
taskRepo.PushDelayed(ctx, "GENERATE_MASTER", taskID, runAt)
dueTaskIDs, _ := taskRepo.MoveDelayedToReady(ctx, "GENERATE_MASTER", time.Now())
````

---

### `internal/providers`

**Purpose**: External service integrations

**Key files**:
- `redis_provider.go`: Redis client initialization (wraps `github.com/go-redis/redis/v9`)
- `uploader.go`: Artifact storage (local filesystem)

**Example**:
````go
redisClient := providers.NewRedisClient(config)
uploader := providers.NewLocalUploader(config.LocalArtifactsDir)

url, err := uploader.Upload(ctx, "output.json", []byte(`{"ok": true}`))
````

---

### `internal/backoff`

**Purpose**: Retry delay computation

**Key files**:
- `backoff.go`: Backoff policy implementations

**Policies**:
- `fixed`: Constant delay
- `linear`: `baseSeconds * attempt`
- `exponential`: `baseSeconds * 2^attempt`
- `exp_full_jitter`: Exponential with random jitter [0, delay]
- `exp_equal_jitter`: Exponential with half delay + jitter [0, half]

**Example**:
````go
delay := backoff.ComputeBackoff("exp_full_jitter", 5, 900, 3)
// Returns delay for attempt 3 with base 5s, max 900s
````

**See**: `docs/11-backoff.md` for backoff details

---

## CLI Package (`cmd/codeq`)

### `cmd/codeq/main.go`

**Purpose**: Command-line interface (CLI) for local development and testing

**Commands**:
- `codeq init`: Generate config template
- `codeq auth login|set|show|clear`: Manage authentication tokens
- `codeq task create|get|result`: Task operations
- `codeq worker start`: Start worker loop
- `codeq queue inspect`: View queue stats

**Example**:
````bash
codeq auth set --token <producer-token>
codeq task create --command GENERATE_MASTER --payload '{"jobId":"j-1"}'
codeq worker start --commands GENERATE_MASTER --poll-interval 5s
````

**See**: `docs/15-cli-reference.md` for full CLI reference

---

## Testing

### `pkg/app/integration_test.go`

Full integration tests covering:
- Task creation, claim, completion flow
- NACK and backoff
- Webhook subscriptions
- Admin operations

### `internal/repository/task_repository_test.go`

Unit tests for repository layer (requires Redis)

### `test/local_flow.py`

Python script for manual end-to-end testing

---

## Build and Deployment

### Helm Chart (`helm/codeq/`)

Kubernetes deployment configuration. See `helm/codeq/README.md` for usage.

### GitHub Workflows (`.github/workflows/`)

CI/CD automation. See `docs/16-workflows.md` for workflow details.

---

## Further Reading

- **Domain model**: `docs/02-domain-model.md`
- **Architecture flows**: `docs/03-architecture.md`
- **HTTP API**: `docs/04-http-api.md`
- **Queue semantics**: `docs/05-queueing-model.md`
- **Storage layout**: `docs/07-storage-kvrocks.md`
- **Security**: `docs/09-security.md`
- **Configuration**: `docs/14-configuration.md`
- **Contributing**: `CONTRIBUTING.md`
