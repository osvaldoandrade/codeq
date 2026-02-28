# Guide 4: Build Performance Optimization

## Current Build Time Baseline

Run build steps to understand baseline:
```bash
.github/actions/daily-perf-improver/build-steps/action.yml
```

Typical timings:
- Go build: ~5-10 seconds
- Unit tests: ~15-30 seconds
- Benchmarks: ~30+ seconds (depends on benchtime)
- K6 smoke test: ~15 seconds (skipped if server not running)

## Go Build Optimization

**Use build cache effectively:**
```bash
# First build (no cache)
time go build -o ./tmp/codeq-server ./cmd/server

# Second build (should be fast with cache)
time go build -o ./tmp/codeq-server ./cmd/server

# Clear cache and rebuild
go clean -cache
time go build -o ./tmp/codeq-server ./cmd/server
```

**Measure rebuild after small change:**
```bash
# Verify cache works (touch one file, rebuild should be fast)
touch internal/repository/task_repository.go
time go build -o ./tmp/codeq-server ./cmd/server
```

## Test Execution Optimization

**Identify slow tests:**
```bash
go test -v -timeout=30s ./internal/repository ./internal/services 2>&1 | grep -E "^(ok|FAIL)" | sort
```

**Parallel test execution:**
```bash
# Default parallelism (usually number of cores)
go test ./...

# Force serial (slow, for debugging)
go test -parallel=1 ./...

# Increase parallelism (if cores available)
go test -parallel=16 ./...
```

**Skip expensive tests during iteration:**
```bash
# Skip integration tests, run unit tests only
go test -short ./internal/services
```

## Benchmark Performance Tips

**Baseline benchmark:**
```bash
go test -bench . -benchtime=30s -run=^$ ./internal/bench > baseline.txt
```

**Avoid:intimidating benchmarks:**
- Don't use very high `-benchtime` during iteration (10s is usually enough)
- Discount first run (CPU hasn't warmed up); run 3x and use middle result
- Disable GC if measuring tight loops: `-run=^$ -benchmem`

## CI Pipeline Optimization

**Recommendations for Phase 3 workflows:**

1. **Parallel steps:** Go build and unit tests can run in parallel
   - Build produces binary
   - Tests use Go cache independently
   - No file conflicts

2. **Incremental testing:** Skip benchmarks on every push
   - Run benchmarks only on merge to main
   - Run in nightly scheduled job for regression detection

3. **Cache management:**
   - Keep Go build cache between runs: `actions/setup-go@v5` does this
   - Cache K6 dependencies if using pre-built binary

## Profiling Build Performance

**Identify slow build stages:**
```bash
# Show dependency build times
go build -v ./cmd/server 2>&1 | tee build.log
# Look for long-running imports

# Build with timing
go build -work ./cmd/server
# Keeps temp build files; examine .go files compiled
```

## Docker Build Optimization

If building Docker image:
```dockerfile
# Bad: rebuilds all on code change
FROM golang:1.23
COPY . /app
RUN go build -o app ./cmd/server

# Better: leverage cache
FROM golang:1.23 as builder
COPY go.mod go.sum /app/
WORKDIR /app
RUN go mod download
COPY . /app
RUN go build -o app ./cmd/server

FROM scratch
COPY --from=builder /app/app /app
CMD ["/app"]
```

## Commands to Integrate

Add to your workflow for phase 3:
```bash
# Time critical path
/usr/bin/time -v go build -o ./tmp/codeq-server ./cmd/server
/usr/bin/time -v go test -parallel=4 ./...
```

This shows: elapsed time, CPU usage, memory peak, page faults (I/O operations).
