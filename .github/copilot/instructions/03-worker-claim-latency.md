# Worker and Task Claim Latency Optimization

## Overview
Worker claim operations are critical to throughput. Target: P99 ≤ 100ms. Focus on reducing claim path latency through data access patterns and batching.

## Task Claim Lifecycle

### Current Flow
1. Worker requests available tasks
2. Redis: SCAN for pending tasks
3. Redis: HGET task metadata
4. Redis: UPDATE task status to "claimed"
5. Return task to worker

Each step can be optimized.

## Latency Analysis

### Identify Bottlenecks
1. Profile claim operation:
```bash
go test -bench=BenchmarkClaim -cpuprofile=claim.prof -benchmem ./internal/bench
go tool pprof claim.prof
# Look for: SCAN time, HGET roundtrips, Lua script overhead
```

2. Check network characteristics:
```bash
# Measure RTT to Redis
redis-cli --latency-history
```

3. Monitor under load:
```bash
k6 run loadtest/k6/claim-latency.js
# Review: P50, P95, P99 latencies per K6 output
```

## Optimization Strategies

### 1. Reduce Network Roundtrips
```go
// ❌ Multiple RTTs
task := rdb.HGet(ctx, key, "data")
rdb.HSet(ctx, key, "status", "claimed")

// ✅ Single RTT with Lua script
const claimScript = `
local task = redis.call('HGET', KEYS[1], 'data')
redis.call('HSET', KEYS[1], 'status', 'claimed')
return task
`
task := rdb.Eval(ctx, claimScript, []string{key})
```

### 2. Batch Claim Operations
```go
// For worker requesting multiple tasks
pipe := rdb.Pipeline()
for i := 0; i < batchSize; i++ {
    key := getPendingTaskKey(i)
    pipe.HGet(ctx, key, "data")
    pipe.HSet(ctx, key, "status", "claimed")
}
results, _ := pipe.Exec(ctx)
// Single RTT for all claims
```

### 3. Use Sorted Sets for Efficient Ordering
```go
// Instead of SCAN with filtering, use pre-sorted data
// ZRANGEBYSCORE for priority-ordered tasks (already in codeQ design)
tasks := rdb.ZRangeByScore(ctx, queueKey, 
    &redis.ZRangeByScore{Min: "-inf", Max: strconv.Itoa(currentTime)})
// O(log N + M) instead of O(N) SCAN
```

### 4. Connection Warmup and Pooling
```go
// Ensure connection pool pre-warmed before peak traffic
for i := 0; i < connPoolSize; i++ {
    rdb.Ping(ctx)  // Establish and cache connections
}
```

## Caching Strategy

### Task Metadata Caching
For frequently accessed tasks:
```go
type TaskCache struct {
    mu    sync.RWMutex
    cache map[string]*Task
    ttl   time.Duration
}

func (tc *TaskCache) Get(id string) *Task {
    tc.mu.RLock()
    defer tc.mu.RUnlock()
    if t, ok := tc.cache[id]; ok {
        return t
    }
    return nil
}
```

⚠️ **Caution**: Caching adds complexity. Only use if:
- Read:write ratio > 5:1
- Metadata changes infrequently
- Cache invalidation is manageable

## Measurement Approach

### 1. Baseline Measurement
```bash
# Run claim benchmark 5 times, record results
for i in {1..5}; do
    go test -bench=BenchmarkClaim -benchmem -benchtime=10s \
        -run=BenchmarkClaim ./internal/bench | grep Benchmark
done
# Average the ns/op values
```

### 2. Load Test Baseline
```bash
cd loadtest
k6 run -u 10 -d 30s k6/claim-latency.js > baseline.json
# Extract: P50, P95, P99 from summary
```

### 3. Implement Optimization
Apply one optimization, rebuild, re-measure

### 4. Compare Results
```bash
# Target: 30-50% reduction in P99 latency
# Minimum: No regression in throughput (ops/sec maintained)
```

## Concurrency Considerations

### Lock Contention
If claim path has mutexes:
1. Profile with go tool pprof to identify contention
2. Use sync.RWMutex for read-heavy operations
3. Consider lock-free data structures for hot paths

### Goroutine Coordination
```go
// Batch processing reduces goroutine overhead
// Instead of 1 goroutine per claim:
claims := make([]Task, 0, 100)
for len(claims) < maxBatchSize && hasMoreTasks() {
    claims = append(claims, claimNextTask())  // Process batch
}
// Reduces synchronization overhead
```

## Success Metrics
- ✅ P99 claim latency < 100ms (sustained)
- ✅ P50 latency < 20ms
- ✅ Throughput maintained or improved (ops/sec)
- ✅ No increase in error rate or timeouts
- ✅ Memory usage stable under load
- ✅ GC pause time < 50ms

## Testing with Load Tests
```bash
# Use k6 scenarios from docs/26-load-testing.md
cd loadtest
k6 run -u 20 -d 60s k6/claim-latency.js
# Verify improvements in aggregated metrics
```
