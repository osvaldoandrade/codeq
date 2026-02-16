# Test Coverage Documentation

## Overview

This document describes the test coverage strategy for the codeq project.

## Current Coverage

### Overall Testable Packages: 61.8%

| Package | Coverage | Status |
|---------|----------|--------|
| internal/backoff | 96.0% | ✅ Excellent |
| internal/domain | 100.0% | ✅ Complete |
| internal/repository | 72.8% | ✅ Good |
| internal/services | 66.7% | ✅ Good |
| internal/providers | 76.9% | ✅ Good |
| pkg/config | 0.0% | ⚠️ Hard to test (env vars) |

## Test Infrastructure

### Testing Tools

- **Testing Framework**: Go's built-in `testing` package
- **Redis Mocking**: `miniredis` for in-memory Redis simulation
- **Coverage Tool**: Go's built-in `cover` tool
- **CI**: GitHub Actions workflow for automated coverage reporting

### Test Patterns

All tests follow consistent patterns:

1. **Table-Driven Tests**: Used for testing multiple scenarios
2. **Setup Helpers**: `setupXxx()` functions with `t.Helper()` for consistent test setup
3. **Miniredis**: Used for all Redis-dependent tests
4. **Context**: All tests use `context.Background()`
5. **Cleanup**: `t.Cleanup()` for proper resource cleanup

## Running Tests

### Run All Tests

```bash
go test ./...
```

### Run Testable Packages Only

```bash
go test ./internal/backoff ./internal/repository ./internal/services ./internal/providers ./pkg/domain ./pkg/config
```

### Run with Coverage

```bash
go test -coverprofile=coverage.out ./internal/backoff ./internal/repository ./internal/services ./internal/providers ./pkg/domain ./pkg/config
go tool cover -html=coverage.out  # View in browser
```

### Run Specific Package

```bash
go test -v -cover ./internal/backoff
go test -v -cover ./internal/repository
go test -v -cover ./internal/services
```

## Test Coverage by Component

### Backoff Package (96%)

Tests cover all backoff policies:
- Fixed delay
- Linear backoff
- Exponential backoff
- Exponential with equal jitter
- Exponential with full jitter
- Edge cases (negative attempts, zero/negative base/max)

See: `internal/backoff/backoff_test.go`

### Domain Package (100%)

Complete coverage of:
- Command marshaling (Binary and Text)
- TaskStatus marshaling (Binary and Text)
- All domain model structs and fields
- Type assertions and conversions

See: `pkg/domain/domain_test.go`

### Repository Layer (72.8%)

**Task Repository**:
- Enqueue with idempotency
- Idempotency Bloom filter optimization (skip negative GET)
- Ghost Bloom filter optimization (skip HGET for deleted tasks)
- Priority-based claim
- Nack with DLQ handling
- Delayed queue scheduling
- Lease expiry and cleanup
- Heartbeat and abandon
- Admin queues and stats

**Result Repository**:
- Task retrieval
- Result save and get
- Task status updates
- In-progress queue management
- Base64 decoding

**Subscription Repository**:
- Create subscriptions
- Heartbeat to extend TTL
- List active subscriptions
- Notification throttling
- Round-robin group delivery
- Cleanup expired subscriptions

See: `internal/repository/*_test.go`

**Bloom Filter Optimization Tests**:

The repository includes specific tests for both Bloom filter optimizations:

1. **`TestEnqueueIdempotentBloomSkipsNegativeGet`** (`task_repository_test.go:83`):
   - Validates that first-time idempotency keys skip Redis GET entirely
   - Uses `redisCmdCountHook` to verify zero GET operations on first enqueue
   - Confirms fallback to Redis GET on subsequent duplicate enqueue
   - Tests the idempotency Bloom filter fast-path optimization

2. **`TestClaimGhostBloomSkipsHGet`** (`task_repository_test.go:143`):
   - Simulates administratively deleted task (HDEL) with stale ID in queue
   - Uses `redisCmdCountHook` to verify HGET is skipped after ghost filter learns the deletion
   - Tests that ghost Bloom filter reduces wasted HGET operations during Claim
   - Validates queue cleanup (SREM) when ghost task is detected

These tests ensure the Bloom filter optimizations deliver measurable Redis operation reductions without compromising correctness.

### Services Layer (66.7%)

**Scheduler Service**:
- Task creation with validation
- Webhook URL validation
- Idempotent task creation
- Task claiming with wait
- Priority handling
- Heartbeat and abandon
- Nack with backoff
- Admin operations
- Default value handling

**Subscription Service**:
- Create with validation
- Callback URL validation
- Delivery mode validation
- Default event types
- Group mode requirements
- Heartbeat

**Result Callback Service**:
- Constructor defaults
- Backoff delay calculation
- Empty webhook handling
- Webhook dispatch (async)

**Results Service**:
- Get task and result
- Error handling for not found

**Notifier Service**:
- Constructor defaults
- Queue ready notifications
- Subscription filtering

See: `internal/services/*_test.go`

### Providers Layer (76.9%)

**Local Uploader**:
- File upload to local filesystem
- Directory creation
- Path handling

**Redis Provider**:
- Client initialization

See: `internal/providers/providers_test.go`

## CI Integration

### Coverage Workflow

The project includes a GitHub Actions workflow (`.github/workflows/test-coverage.yml`) that:

1. Runs on every push and pull request
2. Tests all testable packages
3. Generates coverage reports
4. Checks if coverage meets 70% threshold
5. Uploads coverage artifacts
6. Adds coverage summary to PR checks

### Workflow Features

- **Coverage Report**: Displays top 20 functions and total coverage
- **Threshold Check**: Warns if coverage is below 70%
- **Artifacts**: Coverage files retained for 30 days
- **Summary**: Coverage percentage shown in GitHub Actions summary

## Best Practices

### Writing Tests

1. **Use Table-Driven Tests** for multiple scenarios
2. **Test Error Cases** as well as happy paths
3. **Use Miniredis** for Redis-dependent tests
4. **Clean Up Resources** with `t.Cleanup()`
5. **Test Edge Cases** (nil, empty, negative values)
6. **Follow Naming Conventions**: `Test<Function><Scenario>`

### Example Test

```go
func TestCreateTaskSuccess(t *testing.T) {
    ctx, _, _, _, svc := setupSchedulerTest(t)
    
    task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "https://example.com/webhook", 3, "", time.Time{}, 0)
    
    if err != nil {
        t.Fatalf("CreateTask failed: %v", err)
    }
    if task == nil {
        t.Fatal("Expected task to be non-nil")
    }
    if task.Command != domain.CmdGenerateMaster {
        t.Errorf("Expected command %s, got %s", domain.CmdGenerateMaster, task.Command)
    }
}
```

## Future Improvements

To reach 70% overall coverage:

1. **Add More Service Tests**: Cover remaining service methods
2. **Add Provider Tests**: If additional providers are added
3. **Integration Tests**: Add more end-to-end tests where possible

## Performance and Load Testing

In addition to unit and integration tests, codeQ includes comprehensive performance and load testing capabilities:

### Load Testing Framework

A complete k6-based load testing suite is available in `loadtest/k6/` with six pre-built scenarios:

1. **Sustained throughput** (`01_sustained_throughput.js`): 500 tasks/sec for extended periods
2. **Burst load** (`02_burst_10k_10s.js`): 10,000 tasks in 10 seconds
3. **Many workers** (`03_many_workers.js`): 100+ concurrent worker instances
4. **Large queue depth** (`04_prefill_queue.js`): 100K+ pending tasks
5. **Mixed priorities** (`05_mixed_priorities.js`): Priority distribution testing
6. **Delayed tasks** (`06_delayed_tasks.js`): Delayed task scheduling

**Running load tests:**

````bash
# Start codeQ with dependencies
docker compose up -d

# Run a scenario
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Customize with environment variables
RATE=1000 DURATION=10m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
````

### Go Benchmarks

Fast in-memory benchmarks for regression testing are available in `internal/bench/`:

````bash
# Run all benchmarks
go test ./internal/bench -bench . -benchtime=30s

# Run specific benchmark
go test ./internal/bench -bench BenchmarkCreateTask -benchtime=30s
````

These benchmarks use miniredis for isolated, repeatable performance measurements and are useful for:
- Comparing performance between branches
- Detecting regressions in critical paths
- Quick feedback during development

For comprehensive documentation, see:
- [`docs/26-load-testing.md`](26-load-testing.md) - Complete load testing guide
- [`loadtest/README.md`](../loadtest/README.md) - Scenario documentation
- [`docs/17-performance-tuning.md`](17-performance-tuning.md) - Performance optimization guide

## Notes

- The `pkg/config` package is difficult to test due to environment variable parsing
- CLI tests are run separately in the release workflow
- Focus is on testable business logic: backoff, repository, services, domain, providers
