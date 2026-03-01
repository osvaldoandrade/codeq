# Build Performance and Developer Experience Guide for CodeQ

## Overview

CodeQ build and test execution should be fast to enable rapid iteration. This guide covers build optimization, test execution performance, and developer workflow acceleration.

## Build Times

### Current baseline
- **Full binary build**: ~5-10 seconds
- **Unit tests**: ~10-15 seconds  
- **Go benchmarks**: ~5-30 seconds (configurable)
- **Go vet + fmt check**: ~3-5 seconds

### Optimize for iteration
```bash
# Incremental build (only recompile changed packages)
go build -o server ./cmd/server  # Uses Go cache automatically

# Run a specific test (replace <TestName> with an existing test, e.g., TestEnqueueIdempotent)
go test -v ./internal/repository -run TestEnqueueIdempotent

# Single benchmark (skip others)
go test ./internal/bench -bench BenchmarkHTTP_CreateClaimComplete -benchtime=5s

# Skip tests that require Docker/K6
go test ./cmd/... -short  # Runs only short-duration tests
```

## Parallelizing Development Work

### Use multiple shells
```bash
# Shell 1: Watch and rebuild on changes
go build -o server ./cmd/server && echo "Build succeeded"

# Shell 2: Run quick benchmarks
watch -n 5 'go test ./internal/bench -bench BenchmarkCreateTask -benchtime=5s'

# Shell 3: Run k6 scenario with fixed config
docker compose up -d && \
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

### Use cache to speed up CI
Go's build cache (`GOCACHE`) is automatically used. On fresh checkout, set:
```bash
export GOCACHE=/tmp/go-cache
```

## Test Execution Performance

### Run only relevant tests
```bash
# Test only packages you modified
go test ./internal/repository ./pkg/config

# Run tests matching a pattern
go test -run TestCreate ./internal/...

# Skip long-running tests (benchmarks, integration tests)
go test -short ./...
```

### Parallelize test execution
```bash
# Allow up to 4 tests per package to run in parallel (requires t.Parallel())
go test -parallel 4 ./...

# Control how many packages are tested in parallel
go test -p 4 ./...
```

### Check test dependencies
If tests run slow, profile them:
```bash
go test ./internal/bench -timeout=20s -v 2>&1 | grep -E "^--- PASS|^--- FAIL|^PASS|^FAIL|Run"
```

## Benchmarking for Development

### Quick baseline (5 seconds)
```bash
go test ./internal/bench -bench . -benchtime=5s
```

### Medium confidence (10-15 seconds)
Use for regression detection in PRs:
```bash
go test ./internal/bench -bench . -benchtime=10s
```

### High confidence (30+ seconds)
Use for final validation before release:
```bash
go test ./internal/bench -bench . -benchtime=30s -count=3
```

## Profiling Without Full Load Tests

### Fast CPU profile (in-process benchmark)
```bash
go test ./internal/bench -bench BenchmarkFullWorkflow -benchtime=10s -cpuprofile=cpu.prof
go tool pprof cpu.prof
```

Runs in ~15 seconds; avoids Docker/K6 setup.

### Check memory allocations quickly
```bash
go test ./internal/bench -bench . -benchmem -benchtime=5s
```

Look for high B/op (bytes per operation) indicating unnecessary allocations.

## Static Analysis Performance

### Check formatting only (no fix)
```bash
gofmt -l .  # List files; fast
# vs
gofmt -w .  # Rewrite files; slower on large codebases
```

### Run vet in parallel
```bash
go vet ./...  # Automatically runs in parallel
```

## Common Developer Tasks

### Scenario: Testing a task repository optimization
1. Make code change in `internal/repository/task_repository.go`
2. Run quick benchmark: `go test ./internal/bench -bench BenchmarkHTTP_CreateClaimComplete -benchtime=5s`
3. If promising, run full benchmark: `go test ./internal/bench -bench . -benchtime=15s`
4. If latency improves, start k6 load test for realistic validation

### Scenario: Debugging allocations
1. Identify benchmark with high B/op: `go test ./internal/bench -benchmem`
2. Profile memory: `go test ./internal/bench -bench . -memprofile=mem.prof`
3. Inspect allocations: `go tool pprof -alloc_objects mem.prof`
4. Look at source code: `(pprof) list FunctionName`

### Scenario: Fast CI feedback
Use shorter benchmarks in PR checks:
```bash
# In .github/workflows/test-coverage.yml
go test ./internal/bench -bench . -benchtime=5s -timeout=30s
```

Allow longer benchmarks only on `main` branch for thorough validation.

## Performance Engineering Workflow Summary

```
Code Change
    ↓
go test (unit tests) [10s]
    ↓
go test -bench (quick benchmark) [5-10s]
    ↓
(if promising)
    ↓
go test -bench -benchtime=15s (medium benchmark) [15s]
    ↓
(if latency improves)
    ↓
docker compose up && k6 scenario [2-5min]
    ↓
Success: Create PR
```

Total time for the inner fast feedback loop (code change → unit tests → quick benchmark, skipping medium benchmark and k6): ~20–30 seconds.
Total time for the full workflow including the optional k6 scenario: ~2–5 minutes.
