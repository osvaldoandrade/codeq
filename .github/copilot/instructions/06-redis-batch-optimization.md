# Redis Batch Optimization - Pipelining in Queue Statistics

## Overview
This guide documents the batch pipelining optimization applied to queue statistics operations. Consolidating multiple Redis calls into single pipeline batches reduces round-trip times (RTTs) from N calls to 1-2 RTTs.

## Optimization Applied

### AdminQueues (80+ calls → 1-2 RTT)
**Location**: `internal/repository/task_repository.go:955-1010`

**Before**: Loop through commands, then loop through priorities, executing individual Redis commands:
```
for each command:
  for each priority:
    LLen (1 RTT)
  SCard (1 RTT)
  ZCard (1 RTT)
  SCard (1 RTT)
Total: 80+ individual RTTs for typical setup
```

**After**: Single pipeline batch:
```
Build pipeline with all LLen, SCard, ZCard operations
Execute all in 1-2 RTTs
Map results back to keys
```

**Impact**: 80-90% RTT reduction (40+ RTTs → 1-2 RTTs)

### QueueStats (10-13 calls → 1 RTT)
**Location**: `internal/repository/task_repository.go:1020-1089`

**Before**: Loop through all priorities, then individual commands:
```
for each priority:
  LLen (1 RTT)
SCard (1 RTT)
ZCard (1 RTT)
SCard (1 RTT)
Total: 10-13 individual RTTs
```

**After**: Single pipeline execution:
```
Batch all LLen calls for priorities
Batch SCard, ZCard, SCard
Execute all in 1 RTT
Extract and sum results
```

**Impact**: 90-92% RTT reduction (10-13 RTTs → 1 RTT)

### PendingLength (10 calls → 1 RTT)
**Location**: `internal/repository/task_repository.go:1091-1113`

**Before**: Loop through priorities, LLen per priority:
```
for each priority:
  LLen (1 RTT)
Total: 10 individual RTTs
```

**After**: Single pipeline:
```
Batch all LLen calls
Execute in 1 RTT
Sum results
```

**Impact**: 90% RTT reduction (10 RTTs → 1 RTT)

### Result Operations (2 RTTs → 1 RTT each)

#### SaveResult
**Location**: `internal/repository/result_repository.go:61-93`
- Before: HSET result, then HGET task, then HSET task (2-3 RTTs)
- After: HSET + HGET in one batch, then HSET task (2 RTTs total, 1 RTT savings on read)

#### UpdateTaskOnComplete
**Location**: `internal/repository/result_repository.go:109-138`
- Before: HGET task, then HSET task, then ZADD TTL (2 RTTs)
- After: HSET + ZADD in one batch (1 RTT for cleanup ops)

#### RemoveFromInprogAndClearLease
**Location**: `internal/repository/result_repository.go:140-149`
- Before: SREM inprog, then DEL lease (2 RTTs)
- After: SREM + DEL in one batch (1 RTT)

**Impact**: 50% reduction on result cleanup path

## Performance Measurement Strategy

### 1. Baseline Measurement (Before Optimization)
```bash
# Run unit tests to ensure functionality
go test ./internal/repository -count=3 -v

# Run benchmarks if available
go test -bench=. -benchmem -benchtime=5s ./internal/bench 2>&1 | grep -E "^BenchmarkAdmin|^BenchmarkQueue|^BenchmarkSave"

# Record baseline numbers in build-steps.log
echo "Baseline measurements captured"
```

### 2. Load Test Measurement
```bash
# Use k6 for realistic load patterns
cd loadtest

# Measure queue stats endpoint performance
k6 run -u 5 -d 30s k6/admin-queues-latency.js > baseline.json

# Measure task claim latency
k6 run -u 10 -d 30s k6/claim-latency.js >> baseline.json

# Extract P95 and P99 latencies from results
```

### 3. Redis Connection Analysis
```bash
# Measure actual RTT
redis-cli --latency-history

# Monitor pipeline batching
redis-cli CLIENT LIST | grep -E "cmd|name"

# Expected: fewer active commands with pipelining
```

## Validation Approach

### Correctness
- ✅ All operations return same results (map[string]any, QueueStats, int64)
- ✅ Error handling preserved (redis.Nil, other errors)
- ✅ Type conversions validated (int64 from Val())

### Performance
- Expected 3-5x throughput increase from pipelining
- Latency reduction: 50-90% depending on operation
- No regression in memory usage (pipelines buffered in go-redis)

### Testing
```bash
# Run existing repository tests
go test ./internal/repository -count=3 -race

# Verify no functional changes
go test ./internal/services -count=3 -race
```

## Known Limitations

### Pipeline Buffering
- Larger pipelines (50+ commands) buffer in memory
- Trade-off: slightly higher memory usage for dramatic latency reduction
- Typical pipelines are 10-50 commands, acceptable overhead

### Error Handling
- Must check each result individually for errors
- Pipeline stops on first error in some scenarios
- Code properly extracts errors from each Cmd result

## Future Optimization Opportunities

1. **Lua Scripts**: Combine logic at Redis level
   - Admin queue cleanup with transactional guarantees
   - Result completion as single atomic operation

2. **Connection Pool Tuning**: Increase pool size for high concurrency
   - Current: PoolSize=10, MinIdleConns=0
   - Recommended: PoolSize=20-30 for 100+ concurrent workers

3. **Caching Layer**: Cache queue stats for brief period
   - Only if read:write ratio > 5:1
   - Add cache invalidation on queue changes

## Reproducibility

### Build and Test
```bash
# Build with optimizations
go build -v ./...

# Run tests
go test ./internal/repository ./internal/services -count=1

# Format code
gofmt -w ./internal/repository
```

### Performance Verification
```bash
# Run benchmarks to observe improvements
go test -bench=. -benchmem -benchtime=10s ./internal/bench

# Compare before/after with pprof
go tool pprof -http=:8080 cpu.prof
```

## Implementation Notes

- Pipeline creation and execution properly ordered
- Results mapped back to keys using index tracking
- Error handling checks both cmd.Err() and cmd.Val()
- Type assertions validated before use
- Formatting applied with gofmt for consistency

## References

- Redis Pipelining Guide: `.github/copilot/instructions/02-redis-pipelining.md`
- Worker Claim Latency: `.github/copilot/instructions/03-worker-claim-latency.md`
- go-redis Pipeline API: https://github.com/redis/go-redis/blob/master/pipeline.go
- Performance Baselines: `docs/30-performance-baselines.md`
