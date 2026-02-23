# Worker Claim Latency Analysis Guide

## Critical Path: P99 ≤ 100ms

Worker claim latency is the most user-visible performance metric in codeQ. This guide covers systematic analysis and optimization of the claim path.

## Understanding Claim Latency

### The Claim Flow

1. **Request routing** (1-2ms): HTTP → controller
2. **Queue lookup** (2-5ms): Find available task in Redis
3. **Lease acquisition** (1-2ms): Assign task to worker, set expiration
4. **Serialization** (1-3ms): Marshal task JSON response
5. **Network** (varies): Response to worker

**Target**: < 100ms P99 means tail latency must stay under that threshold under peak load.

## Measurement Strategy

### 1. Synthetic Benchmark (Quick)

Run in-process benchmark to isolate application latency:

```bash
# Run claim benchmark, capture timing data
go test -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=30s -benchmem ./internal/bench | tee bench.txt

# Extract ns/op (nanoseconds per operation = latency)
# Divide by 1,000,000 to get milliseconds
```

**Example output**:
```
BenchmarkScheduler_CreateClaimComplete-4  15284  78256 ns/op
```
This is ~78μs per claim, well under 100ms.

### 2. Load Test with k6 (Realistic)

Run under sustained load to observe P99:

```bash
# Sustained 1000 tasks/sec with 300 workers for 5 minutes
RATE=1000 DURATION=5m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Check k6 output for P99 metrics
# Look for: p(99) under claim operation threshold
```

**What to watch**:
- P99 latency trending upward (indicates scaling issue)
- GC pauses correlating with latency spikes
- Redis connection pool exhaustion

### 3. Profiling Under Load

Capture CPU/memory profiles during k6 run:

```bash
# Terminal 1: Start services
docker compose up

# Terminal 2: Run load test
DURATION=2m docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Terminal 3: Profile during load
go tool pprof -http=:8081 http://codeq:6060/debug/pprof/profile?seconds=60

# Focus on: functions in claim path, memory allocations
```

## Common Latency Bottlenecks

### 1. Redis Lookup Inefficiency

**Symptom**: Increasing P99 with more tasks in queue

**Investigation**:
```bash
# Check queue depth impact
QUEUE_DEPTH=10000 docker compose --profile loadtest run --rm k6 run /scripts/04_prefill_queue.js

# Profile Redis commands in claim path
# Look for: LLEN, LPOP operations that scale with queue size
```

**Fix**: Use O(1) set-based queue structure (already implemented in current codebase).

### 2. GC Pressure from Allocations

**Symptom**: Occasional P99 spikes, GC logs showing frequent collections

**Investigation**:
```bash
# Profile allocations in claim operation
go test -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s -benchmem ./internal/bench | grep Allocs/op

# Generate memory profile
go test -memprofile=mem.prof \
  -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s ./internal/bench

go tool pprof -alloc_objects mem.prof | top -cum
```

**Fix**: Use object pools or sync.Pool for frequently allocated task structs.

### 3. JSON Serialization Overhead

**Symptom**: High CPU usage without obvious queue issues

**Investigation**:
```bash
# Profile CPU during claim-heavy workload
go test -cpuprofile=cpu.prof \
  -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s ./internal/bench

go tool pprof -top cpu.prof | grep -i "json\|unmarshal\|marshal"
```

**Fix**: Consider `sonic` (already imported) for hot paths, pre-allocate buffers.

### 4. Lease Contention

**Symptom**: P99 degrades with more concurrent workers

**Investigation**:
```bash
# Simulate many workers competing for claims
WORKER_VUS=500 DURATION=5m \
  docker compose --profile loadtest run --rm k6 run /scripts/03_many_workers.js

# Check Redis PEXPIRE command latency (lease assignment)
```

**Fix**: Batch lease operations, use Lua scripts for atomic multi-ops.

## Before/After Comparison Protocol

1. **Establish baseline**:
   ```bash
   go test -bench=BenchmarkScheduler_CreateClaimComplete \
     -benchtime=30s -benchmem ./internal/bench > baseline.txt
   ```

2. **Implement optimization** on new branch

3. **Measure after**:
   ```bash
   go test -bench=BenchmarkScheduler_CreateClaimComplete \
     -benchtime=30s -benchmem ./internal/bench > optimized.txt
   ```

4. **Calculate improvement**:
   ```bash
   # Compare ns/op values
   # % improvement = ((baseline - optimized) / baseline) * 100
   ```

## Success Criteria

| Metric | Target | Status |
|--------|--------|--------|
| Claim latency (ns/op) | < 80,000 | Benchmark metric |
| P99 claim (k6) | < 100ms | Load test metric |
| Throughput (tasks/sec) | ≥ 1000 | Sustained |
| GC pause | < 10ms | Reduce allocations |
| Memory per claim | ≤ 1KB | Profile guided |

## Integration with CI

The build-steps action includes profiling commands. Monitor regression:

```bash
# CI should capture baseline metrics on main branch
# New PRs automatically compared against baseline
# Significant regressions flag PR for review
```
