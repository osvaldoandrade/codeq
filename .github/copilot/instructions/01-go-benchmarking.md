# Guide 1: Go Benchmarking & Regression Detection

## Quick Reference

**Run benchmarks:**
```bash
go test -bench . -benchtime=30s -run=^$ ./internal/bench
```

**Compare branches:**
```bash
# On main branch
go test -bench . -benchtime=10s -run=^$ ./internal/bench > /tmp/main.txt

# On feature branch
go test -bench . -benchtime=10s -run=^$ ./internal/bench > /tmp/feature.txt

# Compare with benchstat
go install golang.org/x/perf/cmd/benchstat@latest
benchstat /tmp/main.txt /tmp/feature.txt
```

## Codebase-Specific Patterns

**Key benchmark in this repo:** `internal/bench/http_bench_test.go`
- Tests HTTP request handling through Gin framework
- Simulates actual server request processing
- Uses miniredis for in-process testing (fast, no Docker required)

**Patterns to measure:**
1. Request/response latency (p50, p95, p99)
2. Memory allocations per request
3. Goroutine count during concurrent requests
4. Cache hit rates in task repository operations

## Regression Detection Strategy

1. **Baseline:** Run benchmarks before making changes
   ```bash
   go test -bench . -benchtime=10s ./internal/bench > baseline.txt
   ```

2. **After changes:** Run same benchmark
   ```bash
   go test -bench . -benchtime=10s ./internal/bench > after.txt
   benchstat baseline.txt after.txt
   ```

3. **Thresholds:** Flag if results show >5% regression in latency or >10% increase in allocations

## Common Optimization Targets

- **HTTP handler efficiency:** Reduce allocations in request processing
- **Repository layer:** Cache frequently accessed objects (tasks, subscriptions)
- **Lease repair logic:** O(1) pipelined operations in task_repository.go
- **Concurrent access:** Lock contention in memory plugin during high throughput

## Debugging Slow Benchmarks

Use CPU profiling to identify hotspots:
```bash
go test -bench . -benchtime=5s -cpuprofile=cpu.prof ./internal/bench
go tool pprof cpu.prof
# In pprof: "top" to see hot functions, "list functionName" to see code
```

Memory profiling:
```bash
go test -bench . -benchtime=5s -memprofile=mem.prof ./internal/bench
go tool pprof mem.prof
```
