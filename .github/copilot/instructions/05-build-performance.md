# Build Performance Optimization for Faster Development Cycles

## Overview
Optimize build times and test execution to enable rapid performance iteration. Faster builds = faster hypothesis testing in performance work.

## Build Time Analysis

### Baseline Measurement
```bash
# Measure clean build
time go build -o server ./cmd/server

# Measure incremental build (after small change)
touch internal/services/task_service.go
time go build -o server ./cmd/server
```

### Understanding Go Build Cache
```bash
# Check cache size
go env GOCACHE

# Clear cache if needed
go clean -cache

# View cached packages
find $(go env GOCACHE) -type f | wc -l
```

## Optimization Strategies

### 1. Reduce Unnecessary Rebuilds
```bash
# Only build what's needed
go build -o server ./cmd/server  # Build specific binary
vs
go build ./...  # Rebuilds all packages

# For tests, focus on changed packages
go test ./internal/services/...  # Only test services package
```

### 2. Use Build Flags to Reduce Binary Size
```bash
# Strip symbols and debug info for faster linking
go build -ldflags="-s -w" -o server ./cmd/server

# Smaller binary = faster linking phase
```

### 3. Parallelize Test Execution
```bash
# Run tests in parallel (default is GOMAXPROCS)
go test -parallel 16 ./...

# Specify explicitly if needed
go test -parallel 8 ./internal/...
```

### 4. Cache Test Results
```bash
# Tests are cached automatically if inputs unchanged
# To bypass cache (force rerun):
go test -count=1 ./...

# View cache behavior with -v:
go test -v ./internal/backoff
# Look for "(cached)" in output
```

### 5. Selective Test Coverage
codeQ already implements this - only test non-private packages:
```bash
# Current strategy (efficient)
go test -coverprofile=coverage.out \
  ./internal/backoff \
  ./internal/repository \
  ./internal/services \
  ./pkg/domain \
  ./pkg/config
```

## Benchmark-Focused Build Workflow

### Quick Performance Testing Loop
```bash
# 1. Make code change
# 2. Run focused benchmark
go test -bench=BenchmarkClaim -benchmem -benchtime=3s \
    -run=BenchmarkClaim ./internal/bench

# 3. Compare to baseline (if using benchstat)
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -benchmem > new.txt
benchstat baseline.txt new.txt
```

### One-Liner for Rapid Iteration
```bash
# Profile, benchmark, and compare in one command
go test -bench=. -benchmem -cpuprofile=cpu.prof -memprofile=mem.prof \
  -benchtime=5s -run=^$ ./internal/bench && \
  go tool pprof -top cpu.prof
```

## CI Build Optimization

### Separate Build and Test
```yaml
# Instead of: go build && go test
# Split into:
go build -o server ./cmd/server  # Fast, no tests
go test ./internal/...            # Focused testing
```

### Caching Strategy (Already in use)
- Use Go module cache (go env GOCACHE)
- Cache Go build artifacts between CI runs
- Store benchmark baselines for comparison

## Development Environment Setup

### Recommended Tools
```bash
# Watch-based rebuilds (optional)
go install github.com/cosmtrek/air@latest
# Configure in .air.toml, then: air

# Benchmark comparison
go install golang.org/x/perf/cmd/benchstat@latest

# Memory profiling
go install github.com/DavidGamba/go-getoptions@latest
```

### Hot Reload Development
```bash
# Use air for auto-rebuilding during development
# Configured in .air.toml
air  # Auto-rebuilds on file changes

# Or manual watch:
find . -name "*.go" | entr go build -o server ./cmd/server
```

## Build Step Workflow Integration

### In build-steps/action.yml
The action already includes:
1. ✅ Dependency download and verification
2. ✅ Separate server and CLI builds
3. ✅ Test execution with coverage
4. ✅ Benchmark runs with timing
5. ✅ Code formatting checks
6. ✅ Build time baselines

### Using Build Steps Locally
```bash
# Download and run the build-steps action locally
# (Currently requires actions runner, or manually run commands from action.yml)

# Manual equivalent:
go mod download && go mod verify
go build -v -o server ./cmd/server
go build -v -o codeq ./cmd/cli
go test -v -coverprofile=coverage.out ./internal/backoff ./internal/repository ./internal/services ./pkg/domain ./pkg/config
go test -bench=. -benchmem ./internal/bench
```

## Success Metrics
- ✅ Clean build time < 30 seconds
- ✅ Incremental build time < 5 seconds
- ✅ Test suite runs < 60 seconds for focused packages
- ✅ Benchmark execution < 30 seconds
- ✅ Full build-steps.log generated and reproducible

## Troubleshooting Slow Builds

### Check for slow packages
```bash
go build -v ./cmd/server 2>&1 | grep -E "\.go" | tail -20
# Slow packages appear later in the list
```

### Profile the build itself
```bash
go build -v -work -x ./cmd/server 2>&1 | head -50
# Look for repeated compilation steps
```

### Clear caches if stuck
```bash
go clean -cache
go clean -modcache
go build -v ./cmd/server  # Fresh build
```
