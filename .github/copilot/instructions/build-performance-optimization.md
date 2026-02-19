# Build Performance Optimization Guide

## Development Workflow Performance

Fast build cycles enable rapid iteration on performance improvements. This guide covers optimization of build times and build infrastructure.

## Quick Build Time Measurement

```bash
# Measure current build time
time go build -v ./cmd/server

# Expected baseline: 5-10 seconds (varies by CPU)
```

## Identifying Build Bottlenecks

### 1. Dependency Analysis

```bash
# Show import dependency depth
go mod graph | head -20

# Find heaviest direct dependencies
go list -m all | sort -t: -k2 -rn | head -20
```

**Heavy dependencies impact**:
- Larger binary size
- Longer build time
- More import cycles (slower analysis)

### 2. Build Profile

```bash
# Generate build timing data
go build -v -work ./cmd/server 2>&1 | tee build-log.txt

# Show packages taking longest to compile
grep "^#" build-log.txt | head -20
```

## Common Build Optimizations

### 1. Use `-trimpath` for Reproducible Builds

```go
// In build scripts or Makefile
go build -trimpath -v ./cmd/server
```

**Benefit**: Removes absolute filesystem paths from binary, aids caching.

### 2. Conditional Compilation

Build only needed binaries:

```bash
# Build server only (not CLI tools)
go build -v ./cmd/server

# Build CLI tools only
go build -v ./cmd/codeq
```

### 3. Incremental Builds

Go automatically caches. Force clean rebuild only when necessary:

```bash
# Development: fast incremental
go build ./cmd/server

# CI/Release: full rebuild
go clean -modcache && go build ./cmd/server
```

## Testing Performance

### 1. Parallel Test Execution

```bash
# Run tests in parallel (default uses GOMAXPROCS)
go test -v -timeout 120s -race ./internal/... ./pkg/...

# Explicit parallelism
go test -v -p 4 ./internal/... ./pkg/...
```

### 2. Selective Test Execution

```bash
# Don't test heavy packages (e.g., integration tests)
go test -v -timeout 60s \
  ./internal/backoff \
  ./internal/repository \
  ./internal/services \
  ./pkg/domain \
  ./pkg/config

# Skip integration tests entirely
go test -v -timeout 60s -short ./...
```

### 3. Test Cache Awareness

```bash
# Go caches test results automatically
# Avoid cache misses by minimizing test file changes
go test -v ./internal/bench

# Force cache invalidation only when needed
go clean -testcache && go test -v ./internal/bench
```

## Benchmark Performance

### 1. Benchmark Variance Reduction

```bash
# Longer benchmark runs = more stable results = fewer re-runs
go test -bench=. -benchtime=30s -benchmem ./internal/bench

# Statistical significance takes time
# 5-10s: Quick feedback (high variance)
# 30s: Reliable baseline (low variance)
```

### 2. Comparison Benchmarks

Use benchstat tool for statistical comparison:

```bash
# Install benchstat
go install golang.org/x/perf/cmd/benchstat@latest

# Compare runs
go test -bench=. -benchmem ./internal/bench > new.txt
benchstat old.txt new.txt
```

## CI/CD Build Optimization

### 1. Cache Go Modules

```yaml
# In GitHub Actions workflow
- name: Set up Go
  uses: actions/setup-go@v5
  with:
    go-version-file: go.mod
    # Modules automatically cached
```

### 2. Minimize Checkout Time

```bash
# Shallow clone for CI (if not needed for git history)
git clone --depth 1 https://github.com/osvaldoandrade/codeq
```

### 3. Parallel Jobs

```yaml
# Run tests and benchmarks in parallel
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  
  bench:
    runs-on: ubuntu-latest
    steps:
      - run: go test -bench=. ./internal/bench
```

## Docker Build Optimization

### Multi-stage Builds

```dockerfile
# Stage 1: Builder
FROM golang:1.23 as builder
WORKDIR /build
COPY . .
RUN go build -o server ./cmd/server

# Stage 2: Runtime (minimal)
FROM alpine:latest
COPY --from=builder /build/server /app/server
CMD ["/app/server"]
```

**Benefit**: Final image excludes build dependencies (~1GB reduction).

## Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Build time | < 15s | `time go build ./cmd/server` |
| Test time | < 60s | `time go test ./internal/... ./pkg/...` |
| Binary size | < 50MB | `ls -lh server` |
| Test parallelism | 4+ concurrent | Default Go test runner |
| Cache hit rate | > 90% | Repeated runs without changes |

## Common Performance Pitfalls

1. **Rebuilding when not needed**: Always use incremental builds during development
2. **Synchronous network calls in tests**: Mock external dependencies
3. **Too many test files**: Combine test utilities to reduce import overhead
4. **Large test datasets**: Use small representative data for quick feedback
5. **Blocking on slow tests**: Use `-short` flag or separate integration tests

## Measurement Protocol

```bash
# Baseline
echo "Before optimization:"
time go build -v ./cmd/server
time go test -v -timeout 60s \
  ./internal/backoff \
  ./internal/repository

# After optimization
echo "After optimization:"
time go build -v ./cmd/server
time go test -v -timeout 60s \
  ./internal/backoff \
  ./internal/repository

# % improvement = ((old - new) / old) * 100
```
