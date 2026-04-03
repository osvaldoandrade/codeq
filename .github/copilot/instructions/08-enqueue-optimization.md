# Enqueue Path Optimization via Pipelining

## Overview

The task enqueue operation is a critical hot path in codeQ, executed on every task creation. This guide documents the optimization technique applied to reduce enqueue latency by pipelining multiple Redis operations.

## Problem

The original `enqueueWithID()` function performed three sequential Redis operations:

1. `HSet(ctx, keyTasksHash(), id, json)` - Store task data in main hash
2. `ZAdd(ctx, keyTTLIndex(), id, expireAt)` - Register task for TTL-based cleanup
3. `LPush/ZAdd(ctx, keyQueue(), id)` - Enqueue task to pending or delayed queue

Each operation required a round-trip (RTT) to Redis, totaling **3 RTTs per enqueue request**.

### Performance Impact

- **Before**: 3 × 1ms RTT = 3ms base latency per enqueue
- **Network conditions**: 3 × 5ms RTT = 15ms under high latency (production scenarios)
- **Throughput ceiling**: Limited by RTT serialization

## Solution: Pipeline Batching

All three operations are now batched into a single pipeline, reducing latency by **67% (3 RTTs → 1 RTT)**.

### Implementation Pattern

```go
// Batch all three operations into a single pipeline
pipe := r.rdb.Pipeline()
defer pipe.Close()

pipe.HSet(ctx, taskHashKey, taskID, jsonData)
pipe.ZAdd(ctx, ttlIndexKey, ttlScore, taskID)
pipe.LPush(ctx, queueKey, taskID)  // or ZAdd for delayed tasks

// Single network round-trip
cmds, err := pipe.Exec(ctx)

// Validate each command result
for i, cmd := range cmds {
    if cmd.Err() != nil {
        // Handle error with context about which operation failed
    }
}
```

### Key Benefits

1. **Latency Reduction**: 67% reduction (3 RTTs → 1 RTT)
2. **Throughput Gain**: 3× higher throughput for enqueue operations under network latency
3. **Production Impact**: Especially significant for deployments with higher Redis latency
4. **Backward Compatible**: Zero API changes, fully transparent to callers

## Measurement

### Synthetic Benchmark
Using miniredis (in-process mock Redis):
- Baseline: ~3 sequential calls + overhead
- Optimized: ~1 pipeline call + overhead
- Expected latency reduction: 40-50% in localhost testing
- Production impact: 60-70% with realistic RTT (~5-10ms)

### Where to Observe Improvement

- Prometheus metric: `codeq_task_created_total` latency (if bucket histograms enabled)
- HTTP endpoint: `POST /api/v1/tasks/create` response time
- Load test: k6 create operation latency p50/p95/p99

## Trade-offs

### Advantages
- Simple, localized change
- No behavioral changes
- Consistent error handling with original implementation
- Clear performance benefit

### Considerations
- Slightly higher code complexity (error handling loop)
- Pipeline defers Close() for resource cleanup
- Error messages now include operation index for debugging

## Validation Checklist

- [x] Code compiles without warnings
- [x] Go formatting (gofmt) passes
- [x] All error cases handled with descriptive messages
- [x] Backward compatible - no API changes
- [x] Pipeline properly closed with defer

## Related Optimizations

- **Redis Pipelining Overview**: See `02-redis-pipelining.md` for general pipelining patterns
- **Result Operations**: Similar pipelining applied to `SaveResult`, `UpdateTaskOnComplete`, `RemoveFromInprogAndClearLease`
- **Claim Path**: Already optimized with pipelining in `Claim()` operation
- **Batch Operations**: Reference `06-redis-batch-optimization.md` for larger-scale batching

## Future Opportunities

1. **Idempotency Path**: Pipeline the conditional GETs and SETs for faster idempotent enqueues
2. **Scheduled Tasks**: Further optimize delayed queue registration
3. **Metrics**: Add latency histograms to measure optimization impact in production

## Testing Strategy

### Unit Tests
- Existing tests validate behavior is unchanged
- Test both immediate (pending) and scheduled (delayed) enqueue paths
- Verify error handling for each pipeline operation

### Integration Tests
- Create load test comparing pre/post optimization latency
- Measure p50, p95, p99 latency improvements
- Validate throughput gains under various worker counts

### Production Validation
- Monitor `codeq_task_created_total` before/after deployment
- Compare enqueue endpoint latency percentiles
- Verify no error rate changes
