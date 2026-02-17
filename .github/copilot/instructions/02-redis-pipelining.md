# Redis Pipelining and Connection Optimization

## Overview
codeQ uses go-redis for all Redis operations. Optimize throughput by batching commands and understanding connection pooling behavior.

## Pipelining Basics

### What is Pipelining?
Send multiple commands in one network round-trip instead of individual requests. Reduces latency and increases throughput dramatically.

### Current codeQ Usage
```go
// Single command (current typical pattern)
rdb.HSet(ctx, key, field, value)  // 1 RTT

// Pipelined (batch multiple operations)
pipe := rdb.Pipeline()
pipe.HSet(ctx, key1, field1, value1)
pipe.HSet(ctx, key2, field2, value2)
pipe.HSet(ctx, key3, field3, value3)
cmds, _ := pipe.Exec(ctx)  // 1 RTT for all 3
```

## Performance Impact

### RTT Reduction
- **No pipeline**: 10 operations × 1ms RTT = 10ms
- **With pipeline**: 10 operations × 0.1ms RTT = 1ms (10x improvement)

### Throughput Gain
- Pipeline size 10: ~3-5x throughput increase
- Pipeline size 100: ~20x throughput increase
- Diminishing returns beyond 100-200 commands

## Optimization Opportunities in codeQ

### 1. Bulk Task Claim Operations
```go
// ❌ Current (N RTTs)
for _, task := range tasksToProcess {
    rdb.HGet(ctx, task.QueueKey, "data")
    rdb.HSet(ctx, task.QueueKey, "status", "claimed")
}

// ✅ Optimized (1 RTT)
pipe := rdb.Pipeline()
for _, task := range tasksToProcess {
    pipe.HGet(ctx, task.QueueKey, "data")
    pipe.HSet(ctx, task.QueueKey, "status", "claimed")
}
pipe.Exec(ctx)
```

### 2. Batch Result Storage
When workers complete multiple tasks, pipeline the result writes:
```go
pipe := rdb.Pipeline()
for _, result := range results {
    pipe.HSet(ctx, resultKey(result.ID), "status", result.Status)
    pipe.HSet(ctx, resultKey(result.ID), "data", result.Data)
}
pipe.Exec(ctx)
```

### 3. Scan Operations with Batching
When iterating large datasets:
```go
// Use pipeline within scan loop
pipe := rdb.Pipeline()
for iter := rdb.Scan(ctx, 0, pattern, 100); iter.Next(ctx); {
    pipe.Del(ctx, iter.Val()) // Batch deletions
    if pipe.Len() >= 50 {      // Flush every 50 commands
        pipe.Exec(ctx)
        pipe = rdb.Pipeline()
    }
}
pipe.Exec(ctx)
```

## Connection Pool Tuning

### Current go-redis Defaults
```go
Options: &redis.Options{
    MaxRetries: 3,           // Retry failed commands
    PoolSize:   10,          // Connection pool size
    MinIdleConns: 0,         // Minimum idle connections
}
```

### Optimization Strategy
```go
// For high-concurrency workloads
Options: &redis.Options{
    PoolSize: 20,            // Increase if workers > 10
    MinIdleConns: 5,         // Keep warm connections
    MaxConnAge: time.Hour,   // Refresh old connections
    IdleTimeout: time.Minute, // Close idle connections
}
```

## Measurement Strategy

### Baseline: Individual Commands
```bash
go test -bench=BenchmarkTaskClaim -benchmem -benchtime=10s ./internal/bench
# Record: ops/sec, ns/op, allocs/op
```

### After Pipelining
```bash
go test -bench=BenchmarkTaskClaimPipelined -benchmem -benchtime=10s ./internal/bench
# Compare: ops/sec should increase 3-5x, ns/op should decrease proportionally
```

### Load Test Validation
Use k6 scenarios to validate real-world impact:
```bash
cd loadtest
k6 run k6/claim-latency.js
# Check: P99 latency < 100ms, throughput maintained or improved
```

## Caveats and Trade-offs

### Memory Usage
- Larger pipelines use more memory (buffer all commands)
- Balance: pipeline 50-100 commands, then execute

### Error Handling
- Pipeline collects all errors in slice
- Must check each result for errors individually
```go
pipe := rdb.Pipeline()
pipe.HSet(ctx, k1, f1, v1)
pipe.HSet(ctx, k2, f2, v2)
cmds, err := pipe.Exec(ctx)  // err is first command error
for _, cmd := range cmds {
    if cmd.Err() != nil {  // Check each result
        // Handle error
    }
}
```

## Success Metrics
- ✅ P99 task claim latency < 100ms
- ✅ Throughput increase 3-5x with pipelining
- ✅ Zero increase in memory usage during normal load
- ✅ No degradation in error handling or recovery
