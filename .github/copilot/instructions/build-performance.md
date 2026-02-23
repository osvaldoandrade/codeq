# Build Performance Guide

## Quick Iteration Workflow

### Fast Build-Test-Benchmark Cycle

```bash
# Watch mode: auto-rebuild on file changes
go install github.com/cosmtrek/air@latest
air  # Rebuilds and runs on save

# Or manual rebuild:
go build -o codeq ./cmd/server
go test -bench=BenchmarkScheduler_CreateClaimComplete -benchmem ./internal/bench
```

### Incremental Testing

Don't run full test suite on every change:

```bash
# Test only affected package
go test -v ./internal/repository

# Test with caching (skip unchanged tests)
go test -v ./internal/repository

# Fast test (skip long tests)
go test -short ./...

# Skip expensive packages
go test ./pkg/domain ./pkg/config
```

## CI Build Optimization

### Parallel Test Execution

codeQ tests are isolated and can run in parallel:

```bash
# GitHub Actions: Already parallelizes by default
# Custom CI: Use -parallel flag
go test -parallel 8 -v ./...
```

### Coverage Report Generation (Expensive)

Only generate coverage for affected packages:

```bash
# Instead of:
go test -cover ./...

# Use:
go test -coverprofile=coverage.out \
  ./internal/backoff \
  ./internal/repository \
  ./pkg/config
```

## Dependency Analysis

Monitor import count to keep build fast:

```bash
# Show direct dependencies
go list -m all | wc -l

# Identify heavy dependencies
du -sh $(go list -m -f '{{.Dir}}' all | sort -u) | sort -rn | head
```

## Benchmarking Setup Efficiency

### Miniredis vs Real Redis

Current setup uses miniredis (in-memory) for fast feedback:

```go
// internal/bench/http_bench_test.go
mr, err := miniredis.Run()  // Instant startup, no overhead
```

**Pros:** Instant feedback for local development  
**Cons:** Doesn't test real Redis behavior (network, persistence)

### Baseline Persistence

Store baseline benchmarks in git:

```bash
# Create baseline commit
go test -bench=. -benchmem ./internal/bench > .bench-baseline
git add .bench-baseline
git commit -m "perf: establish benchmark baseline"
```

Then CI can validate against it:

```bash
go test -bench=. -benchmem ./internal/bench | \
  benchstat - .bench-baseline
```

## CPU-Intensive Tasks

### Profile Compilation

```bash
# Build with profiling
go build -cpuprofile=build.prof -o codeq ./cmd/server

# Check what takes time
go tool pprof -top build.prof
```

### Conditional Compilation

If features are conditionally compiled:

```bash
# Fast build without expensive features
go build -tags=noprof -o codeq ./cmd/server
```

## Memory Efficiency During Tests

Benchmarks can consume significant memory. Manage with:

```bash
# Run one benchmark at a time
go test -bench=BenchmarkHTTP -run=^$ ./internal/bench

# Clear test cache to force rerun
go clean -testcache
go test ./...

# Monitor with 'watch'
watch -n 1 'ps aux | grep go'
```

## Reproducible Builds

For consistent benchmarks across environments:

```bash
# Pin Go version
# go.mod already specifies: go 1.23.0

# Pin build timestamp for reproducibility
go build -ldflags "-X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/server
```
