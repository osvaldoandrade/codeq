# Developer Guide

This guide is for developers who want to contribute to codeQ, extend its functionality, or understand its internal architecture.

## Architecture Overview

codeQ follows a layered architecture pattern common in Go web services:

````
┌─────────────────────────────────────────┐
│         HTTP Handlers / CLI             │  (cmd/)
├─────────────────────────────────────────┤
│           Middleware Layer              │  (internal/middleware/)
├─────────────────────────────────────────┤
│          Controller Layer               │  (internal/controllers/)
├─────────────────────────────────────────┤
│           Service Layer                 │  (internal/services/)
├─────────────────────────────────────────┤
│         Repository Layer                │  (internal/repository/)
├─────────────────────────────────────────┤
│     Providers (Redis, Storage)          │  (internal/providers/)
├─────────────────────────────────────────┤
│      Domain Models & Config             │  (pkg/)
└─────────────────────────────────────────┘
````

## Directory Structure

### `pkg/` - Public Packages

Packages in `pkg/` are considered public API and can be imported by external projects.

- **`pkg/domain/`**: Core domain models
  - `task.go`: Task entity and status constants
  - `result.go`: Result entity for completed tasks
  - `subscription.go`: Worker subscription for webhooks
  - `queue_stats.go`: Queue statistics model

- **`pkg/config/`**: Configuration management
  - `config.go`: Application configuration structure and loading

- **`pkg/app/`**: Application bootstrap and wiring
  - `application.go`: Main application setup
  - `url_mappings.go`: Route definitions
  - `integration_test.go`: Integration test suite

### `internal/` - Internal Implementation

Packages in `internal/` are implementation details and cannot be imported by external projects.

#### Controllers (`internal/controllers/`)

Controllers handle HTTP requests and orchestrate service calls. Each controller is focused on a specific API endpoint or group of related endpoints.

**Key controllers:**

- `create_task_controller.go`: Handle task creation (POST /tasks)
- `claim_task_controller.go`: Handle task claiming (POST /tasks/claim)
- `submit_result_controller.go`: Submit task completion results
- `heartbeat_controller.go`: Extend task leases
- `nack_task_controller.go`: Reject and requeue tasks
- `queue_admin_controller.go`: Admin operations on queues
- `create_subscription_controller.go`: Register worker webhooks

**Controller pattern:**

````go
func CreateTaskController(service services.SchedulerService) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // 1. Parse and validate request
        // 2. Extract auth context (from middleware)
        // 3. Call service layer
        // 4. Format and return response
    }
}
````

#### Services (`internal/services/`)

Services contain business logic and orchestrate repository operations.

**Core services:**

- **`scheduler_service.go`**: Main orchestration service
  - Task lifecycle management (create, claim, complete)
  - Queue operations (stats, admin cleanup)
  - Retry and backoff logic
  - Lease expiration handling

- **`results_service.go`**: Result storage and retrieval
  - Store completion results separately from tasks
  - Retrieve results by task ID

- **`subscription_service.go`**: Worker subscription management
  - Register webhook endpoints
  - TTL-based subscription expiration

- **`notifier_service.go`**: Webhook notifications
  - Send worker availability notifications
  - Batch notifications for efficiency

- **`result_callback_service.go`**: Result webhooks
  - Notify producers when tasks complete
  - Handle callback failures

- **`subscription_cleanup_service.go`**: Background cleanup
  - Remove expired subscriptions
  - Runs periodically

**Service pattern:**

````go
type SchedulerService interface {
    CreateTask(ctx context.Context, ...) (*domain.Task, error)
    ClaimTask(ctx context.Context, ...) (*domain.Task, bool, error)
    // ... other methods
}

type schedulerService struct {
    taskRepo repository.TaskRepository
    resultRepo repository.ResultRepository
    // ... dependencies
}
````

#### Repositories (`internal/repository/`)

Repositories handle data persistence using KVRocks (Redis protocol).

**Key repositories:**

- **`task_repository.go`**: Task storage and queuing
  - Implements sorted sets (ZSET) for priority queues
  - Manages delayed tasks using timestamps
  - Handles atomic claim operations
  - DLQ management

- **`result_repository.go`**: Result persistence
  - Store task results as JSON
  - TTL management for result expiration

- **`subscription_repository.go`**: Worker subscription storage
  - Store webhook registrations
  - Command-to-URL mappings
  - TTL-based expiration

**Repository pattern:**

````go
type TaskRepository interface {
    Save(ctx context.Context, task *domain.Task) error
    FindByID(ctx context.Context, id string) (*domain.Task, error)
    Enqueue(ctx context.Context, task *domain.Task) error
    ClaimNextAvailable(ctx context.Context, commands []domain.Command, ...) (*domain.Task, error)
    // ... other methods
}
````

#### Middleware (`internal/middleware/`)

Middleware handles cross-cutting concerns for HTTP requests.

**Authentication middleware:**

- `auth.go`: Producer authentication (access tokens via JWKS)
- `worker_auth.go`: Worker authentication (JWT tokens)
- `any_auth.go`: Accept either producer or worker auth
- `require_admin.go`: Restrict endpoints to admin users
- `worker_scope.go`: Validate worker can access specific resources

**Utility middleware:**

- `logger.go`: Request logging
- `request_id.go`: Generate and propagate request IDs

**Middleware pattern:**

````go
func WorkerAuthMiddleware(jwksURL, issuer string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. Extract token from Authorization header
            // 2. Validate JWT signature and claims
            // 3. Store claims in request context
            // 4. Call next handler
            next.ServeHTTP(w, r)
        })
    }
}
````

#### Providers (`internal/providers/`)

Providers are adapters for external systems and dependencies.

- `redis_provider.go`: Redis/KVRocks connection management
- `uploader.go`: File storage abstraction (for large payloads)

#### Backoff (`internal/backoff/`)

- `backoff.go`: Exponential backoff calculation for retries

### `cmd/codeq/` - CLI Application

The CLI is a standalone application built with Cobra that provides a command-line interface to codeQ.

**Key features:**

- Profile-based configuration management
- Interactive authentication flows
- Rich terminal UI (spinners, progress bars, colored output)
- Supports all API operations

**Main structure:**

````go
func main() {
    rootCmd := &cobra.Command{
        Use:   "codeq",
        Short: "codeQ CLI",
    }
    
    // Add subcommands
    rootCmd.AddCommand(taskCmd())
    rootCmd.AddCommand(queueCmd())
    rootCmd.AddCommand(configCmd())
    // ...
    
    rootCmd.Execute()
}
````

## Key Design Patterns

### Task Lifecycle

````
1. Producer creates task → Store in KVRocks with READY status
2. Add to priority queue (sorted set by priority + timestamp)
3. Worker claims task → Atomic move from ready to in-progress queue
4. Set lease expiration using sorted set with TTL timestamp
5. Worker completes task → Store result, remove from in-progress
6. Optional: Trigger result callback webhook
````

### Queue Structure (KVRocks)

**Task storage:**
- `task:{taskId}`: Hash containing task JSON
- `task:body:{taskId}`: Large payload storage (if needed)

**Queue structures:**
- `q:{command}:ready`: ZSET scored by priority (higher = first)
- `q:{command}:delayed`: ZSET scored by run timestamp
- `q:{command}:inprogress`: ZSET scored by lease expiration
- `q:{command}:dlq`: ZSET for tasks exceeding max attempts

**Atomic claim operation:**

````lua
-- Lua script executed atomically in Redis
1. ZPOPMIN from ready queue (highest priority)
2. Update task status to IN_PROGRESS
3. ZADD to inprogress queue with lease expiration
4. Return task data
````

### Retry and Backoff

When a task is NACKed:

````go
func calculateDelay(attempt int) int {
    // Exponential backoff: 2^attempt seconds, capped at 1 hour
    delay := math.Pow(2, float64(attempt))
    return int(math.Min(delay, 3600))
}
````

Task is moved to delayed queue with `runAt = now + delay`.

### Worker Notifications

**Subscription model:**

1. Worker registers webhook URL for specific commands
2. Subscription stored with TTL (requires periodic heartbeat)
3. When task enqueued, codeQ sends POST to webhook URLs
4. Notification is advisory only (pull model remains authoritative)

**Notification payload:**

````json
{
  "eventType": "WORK_AVAILABLE",
  "command": "GENERATE_MASTER",
  "timestamp": "2026-02-14T20:30:00Z",
  "readyCount": 5
}
````

## Development Workflow

### Setting Up Development Environment

1. **Clone the repository:**

````bash
git clone https://github.com/osvaldoandrade/codeq.git
cd codeq
````

2. **Install dependencies:**

````bash
go mod download
````

3. **Start KVRocks for testing:**

````bash
docker run -d -p 6666:6666 --name kvrocks-dev apache/kvrocks:latest
````

4. **Run tests:**

````bash
# Run all tests
go test ./...

# Run specific package tests
go test ./internal/repository/...

# Run with coverage
go test -cover ./...
````

### Running the Application

**Option 1: Run from source (requires codeq-service repo)**

````bash
cd /path/to/codeq-service
go run main.go
````

**Option 2: Build and run CLI**

````bash
go build -o codeq cmd/codeq/main.go
./codeq --help
````

### Code Style and Conventions

- **Follow Go idioms**: Use `gofmt`, `golint`, and `go vet`
- **Error handling**: Always check errors, wrap with context using `fmt.Errorf`
- **Naming**: Use clear, descriptive names; avoid abbreviations
- **Comments**: Document exported functions and non-obvious logic
- **Testing**: Write unit tests for services and repositories

### Adding a New Feature

Example: Adding a new queue operation

1. **Define domain model** (if needed) in `pkg/domain/`

2. **Add repository method** in `internal/repository/`:

````go
func (r *taskRepository) YourNewOperation(ctx context.Context, ...) error {
    // Implement using Redis commands
}
````

3. **Add service method** in `internal/services/`:

````go
func (s *schedulerService) YourNewOperation(ctx context.Context, ...) error {
    // Business logic
    return s.taskRepo.YourNewOperation(ctx, ...)
}
````

4. **Add controller** in `internal/controllers/`:

````go
func YourNewController(service services.SchedulerService) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Handle HTTP request
    }
}
````

5. **Add route** in `pkg/app/url_mappings.go`:

````go
r.Post("/v1/codeq/your-endpoint", controllers.YourNewController(schedulerService))
````

6. **Add CLI command** in `cmd/codeq/main.go` (optional)

7. **Write tests** and update documentation

### Testing Strategy

**Unit tests:**
- Test services with mock repositories
- Test repositories with mock Redis client or test container

**Integration tests:**
- Located in `pkg/app/integration_test.go`
- Use real KVRocks instance (test container)
- Test complete request flows

**CLI tests:**
- Currently limited to avoid private dependencies
- See `.github/workflows/release.yml` for test scope

## Contributing

1. **Fork the repository**
2. **Create a feature branch**: `git checkout -b feature/your-feature`
3. **Make changes and add tests**
4. **Ensure tests pass**: `go test ./...`
5. **Format code**: `gofmt -s -w .`
6. **Commit with clear messages**
7. **Push and create a pull request**

See [CONTRIBUTING.md](../CONTRIBUTING.md) for detailed guidelines.

## Debugging Tips

### Enable Debug Logging

Set environment variable:

````bash
export LOG_LEVEL=debug
````

### Inspect KVRocks State

Use Redis CLI to inspect data:

````bash
redis-cli -h localhost -p 6666

# List all keys
KEYS *

# Inspect a queue
ZRANGE q:EXAMPLE_TASK:ready 0 -1 WITHSCORES

# Get task details
HGETALL task:550e8400-e29b-41d4-a716-446655440000
````

### Common Issues

**Tasks stuck in inprogress queue:**
- Lease expired but not cleaned up
- Check lease expiration logic in `scheduler_service.go`
- Verify background cleanup job is running

**Webhooks not triggering:**
- Check subscription registration: `GET /v1/codeq/workers/subscriptions`
- Verify webhook URL is reachable
- Check `notifier_service.go` logs

**Authentication failures:**
- Verify JWKS URL is accessible
- Check token issuer and audience claims
- Review middleware logs for JWT validation errors

## Additional Resources

- **[Architecture](03-architecture.md)**: High-level system design
- **[HTTP API](04-http-api.md)**: Complete API reference
- **[Storage Layout](07-storage-kvrocks.md)**: KVRocks data structures
- **[Contributing](../CONTRIBUTING.md)**: Contribution guidelines
- **[Examples](13-examples.md)**: Usage examples

## Questions or Problems?

- Open an issue: https://github.com/osvaldoandrade/codeq/issues
- Check existing docs in `docs/` directory
- Review the source code - it's designed to be readable!
