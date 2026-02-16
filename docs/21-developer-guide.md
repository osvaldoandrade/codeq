# Developer Guide

This guide is for developers who want to contribute to codeQ, extend its functionality, or understand its internal architecture.

## Local Development Setup

The fastest way to start developing codeQ is using Docker Compose with hot reload:

````bash
# Clone and start all services
git clone https://github.com/osvaldoandrade/codeq
cd codeq
docker compose up -d

# Watch logs during development
docker compose logs -f codeq
````

The development environment includes:
- **KVRocks** on port 6666
- **codeQ API** on port 8080 (with hot reload via Air)
- **Automatic recompilation** when you edit Go files in `internal/`, `pkg/`, or `cmd/`

**Making changes:**
1. Edit any Go file
2. Air automatically rebuilds and restarts the server
3. Test your changes immediately at `http://localhost:8080`

**Run tests inside the container:**

````bash
docker compose exec codeq go test ./...
````

**With observability stack (Prometheus + Grafana + Jaeger):**

````bash
docker compose --profile obs up -d
````

For complete details, see [Local Development Guide](22-local-development.md).

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
1. Producer creates task → Store in KVRocks with PENDING status (`codeq:tasks`)
2. Enqueue task ID into a per-command priority pending list (`codeq:q:<command>:pending:<priority>`)
3. Worker claims task → Atomic move from pending list to in-progress set (Lua `RPOP` + `SADD`)
4. Set lease key with TTL (`codeq:lease:<id>` via `SETEX`)
5. Worker completes task → Store result, clear lease, remove from in-progress (`SREM`)
6. Optional: Trigger result callback webhook
````

### Queue Structure (KVRocks)

**Task storage:**
- `codeq:tasks` (hash): field = task ID, value = task JSON.
- `codeq:results` (hash): field = task ID, value = result JSON.
- `codeq:lease:<id>` (string): value = worker ID, TTL = lease duration.
- `codeq:tasks:ttl` (ZSET): retention index (logical TTL).

**Queue structures:**
- `codeq:q:<command>:pending:<priority>` (list)
- `codeq:q:<command>:inprog` (set)
- `codeq:q:<command>:delayed` (ZSET) score = `visibleAt` epoch seconds
- `codeq:q:<command>:dlq` (set)

**Atomic claim operation:**

````lua
-- Lua script executed atomically in Redis
1. RPOP from pending list
2. SADD into in-progress set (skip duplicates if any)
3. Return task ID
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

**Performance and load tests:**
- **k6 load scenarios**: HTTP-level testing with realistic producer and worker traffic patterns
  - Located in `loadtest/k6/`
  - Run via Docker Compose: `docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js`
  - Six pre-built scenarios covering sustained load, bursts, many workers, queue depth, priorities, and delays
- **Go benchmarks**: Fast in-memory benchmarks for regression testing
  - Located in `internal/bench/`
  - Run with: `go test ./internal/bench -bench . -benchtime=30s`
  - Use miniredis for isolated, repeatable performance measurements

See [`docs/26-load-testing.md`](26-load-testing.md) for comprehensive load testing documentation.

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
KEYS codeq:*

	# Inspect queues (example: GENERATE_MASTER)
	LLEN codeq:q:generate_master:pending:0
	SCARD codeq:q:generate_master:inprog
	SMEMBERS codeq:q:generate_master:inprog
	ZRANGE codeq:q:generate_master:delayed 0 -1 WITHSCORES
	SCARD codeq:q:generate_master:dlq
	SSCAN codeq:q:generate_master:dlq 0 COUNT 100

# Get task details
HGET codeq:tasks 550e8400-e29b-41d4-a716-446655440000
````

### Common Issues

**Tasks stuck in inprogress queue:**
- Lease expired but task ID remains in `codeq:q:<command>:inprog`
- Claim-time repair should requeue tasks whose `codeq:lease:<id>` is missing/expired
- Validate worker heartbeats and lease TTLs (`TTL codeq:lease:<id>`)

**Webhooks not triggering:**
- Check subscription registration: `GET /v1/codeq/workers/subscriptions`
- Verify webhook URL is reachable
- Check `notifier_service.go` logs

**Authentication failures:**
- Verify JWKS URL is accessible
- Check token issuer and audience claims
- Review middleware logs for JWT validation errors

## Adding Metrics

codeQ uses Prometheus for observability. Follow these guidelines when adding new metrics:

### Metric Types

**Counters** (always increasing):
- Use for event counts (tasks created, webhooks sent, errors)
- Increment at the point where the event occurs
- Label sparingly to avoid high cardinality

**Histograms** (latency distributions):
- Use for duration measurements (request latency, processing time)
- Record in seconds (not milliseconds)
- Choose appropriate buckets for expected latency range

**Gauges** (instantaneous values):
- Use for current state (queue depth, active connections)
- Prefer custom collectors for values derived from Redis/DB
- Avoid updating gauges on every operation (query on scrape instead)

### Adding a Counter

1. **Define the metric** in `internal/metrics/metrics.go`:

````go
TaskAbandoned = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: namespace,  // "codeq"
        Name:      "task_abandoned_total",
        Help:      "Total number of tasks abandoned by workers.",
    },
    []string{"command"},
)
````

2. **Register the metric** in the `init()` function:

````go
func init() {
    prometheus.MustRegister(
        // ... existing metrics ...
        TaskAbandoned,
    )
}
````

3. **Instrument the code** at the appropriate location (service or repository layer):

````go
// In scheduler_service.go
func (s *schedulerService) AbandonTask(ctx context.Context, req AbandonTaskRequest) error {
    // ... business logic ...
    
    metrics.TaskAbandoned.WithLabelValues(string(task.Command)).Inc()
    
    return nil
}
````

### Adding a Histogram

````go
// 1. Define in metrics.go
WebhookLatencySeconds = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Namespace: namespace,
        Name:      "webhook_latency_seconds",
        Help:      "Webhook HTTP request latency in seconds.",
        Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
    },
    []string{"kind", "outcome"},
)

// 2. Register in init()
func init() {
    prometheus.MustRegister(WebhookLatencySeconds)
}

// 3. Instrument with timing
start := time.Now()
resp, err := http.Post(url, "application/json", body)
duration := time.Since(start).Seconds()

outcome := "success"
if err != nil {
    outcome = "failure"
}
metrics.WebhookLatencySeconds.WithLabelValues(kind, outcome).Observe(duration)
````

### Best Practices

**Label cardinality:**
- Keep label cardinality low (< 1000 unique combinations per metric)
- ❌ Bad: `{task_id="abc-123"}` (unbounded)
- ✅ Good: `{command="GENERATE_MASTER"}` (2 values)
- ❌ Bad: `{worker_id="worker-12345"}` (high cardinality if many workers)
- ✅ Good: `{queue="ready"}` (4 values: ready, delayed, in_progress, dlq)

**Naming conventions:**
- Use `snake_case`: `task_created_total`, not `taskCreatedTotal`
- Counter suffix: `_total` (e.g., `task_created_total`)
- Histogram/Summary suffix: units (e.g., `_seconds`, `_bytes`)
- Gauge: no suffix (e.g., `queue_depth`)

**Instrumentation location:**
- Increment counters close to the event (service or repository layer)
- Avoid instrumenting in controllers (middleware can handle HTTP metrics)
- Record latency at the outermost boundary (end-to-end, not internal steps)

**Testing:**
- Verify metrics are exposed: `curl http://localhost:8080/metrics | grep codeq_`
- Check label values: `codeq_task_created_total{command="GENERATE_MASTER"} 42`
- Test in integration tests if metric is critical

**Documentation:**
- Update `docs/10-operations.md` metric reference table
- Add PromQL examples for new metrics
- Document expected cardinality and scrape performance impact

### Custom Collectors

For metrics derived from external state (Redis, database), use a custom collector instead of updating gauges continuously.

**Example**: The `redisCollector` queries Redis on each scrape instead of updating gauges on every queue operation:

````go
type myCollector struct {
    db   *sql.DB
    desc *prometheus.Desc
}

func (c *myCollector) Describe(ch chan<- *prometheus.Desc) {
    ch <- c.desc
}

func (c *myCollector) Collect(ch chan<- prometheus.Metric) {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    
    value, err := c.queryDatabase(ctx)
    if err != nil {
        return  // Fail silently; scrape continues
    }
    
    ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, value)
}

// Register once at startup
prometheus.MustRegister(&myCollector{db: db, desc: desc})
````

**When to use custom collectors:**
- Queue depths from Redis (avoids updating gauges on every operation)
- Connection pool stats from database drivers
- External system state (Kubernetes, cloud APIs)

**See**: `internal/metrics/redis_collector.go` for a complete example

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
