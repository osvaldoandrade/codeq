# Redis Pipelining Optimization Guide

## Overview

codeQ uses go-redis, which supports automatic pipelining. This guide covers identifying pipelining opportunities and measuring throughput improvements.

## Understanding Current Pipelining

### What go-redis Does Automatically

When you issue multiple commands in quick succession, go-redis batches them:

```go
// These two commands are automatically pipelined
pipe := rdb.Pipeline()
pipe.Get("key1")
pipe.Get("key2")
results, _ := pipe.Exec(ctx)
```

### Where Pipelining Matters

1. **Batch operations**: Claim tasks (multiple queue checks), result submissions
2. **Lookup paths**: Lease verification, task metadata fetch
3. **Update sequences**: Status updates, progress tracking

## Identifying Non-Pipelined Patterns

### ❌ Anti-pattern: Sequential Commands

```go
// BAD: Each command waits for response (round-trip latency multiplied)
val1, _ := rdb.Get(ctx, "key1").Result()
val2, _ := rdb.Get(ctx, "key2").Result()
val3, _ := rdb.Get(ctx, "key3").Result()
```

### ✅ Better: Explicit Pipeline

```go
// GOOD: Commands batched in single round-trip
pipe := rdb.Pipeline()
cmd1 := pipe.Get(ctx, "key1")
cmd2 := pipe.Get(ctx, "key2")
cmd3 := pipe.Get(ctx, "key3")
pipe.Exec(ctx)

val1, _ := cmd1.Result()
val2, _ := cmd2.Result()
val3, _ := cmd3.Result()
```

## Measurement Approach

### 1. Baseline Single Command

```bash
# Measure single Redis command latency
redis-benchmark -h localhost -n 1000 -c 1 get key
```

### 2. Pipeline vs Sequential

Create benchmark comparing patterns:

```go
func BenchmarkRedisSequential(b *testing.B) {
  for i := 0; i < b.N; i++ {
    rdb.Get(ctx, "key1").Result()
    rdb.Get(ctx, "key2").Result()
    rdb.Get(ctx, "key3").Result()
  }
}

func BenchmarkRedisPipelined(b *testing.B) {
  for i := 0; i < b.N; i++ {
    pipe := rdb.Pipeline()
    pipe.Get(ctx, "key1")
    pipe.Get(ctx, "key2")
    pipe.Get(ctx, "key3")
    pipe.Exec(ctx)
  }
}
```

**Expected improvement**: 2-3x faster for 3 commands (reduces 3 round-trips to 1).

### 3. Load Test Impact

Run k6 scenarios with profiling to measure end-to-end impact:

```bash
# Compare sustained throughput before/after pipelining improvements
RATE=1000 DURATION=5m docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Watch for increased tasks/sec and reduced P99 latency
```

## Common Optimization Opportunities

### 1. Claim Path Optimization

In `ClaimTask` (hot path):
- Fetch task metadata from queue
- Verify lease not expired
- Update lease with new expiration
- Return task

**Opportunity**: Pipeline the metadata fetch and lease verification together.

### 2. Batch Lease Renewal

For many workers claiming simultaneously:
- Instead of individual `PEXPIRE` per task
- Use Lua script with multiple keys:

```lua
-- Update multiple lease expirations in single script call
local keys = {...}
for i, key in ipairs(keys) do
  redis.call('PEXPIRE', key, ARGV[1])
end
return #keys
```

### 3. Task Result Storage

When storing result:
- Update task status
- Store result data
- Remove from in-progress set

**Opportunity**: Use Lua script (atomic) or pipeline.

## Best Practices

1. **Batch within transactions**: Use `Lua` scripts for atomic multi-operation sequences
2. **Profile the claim path**: Highest throughput and most latency-sensitive
3. **Measure impact**: Each optimization should show measurable improvement in P99 latency
4. **Monitor in production**: Watch Redis network I/O and command latency metrics

## Success Metrics

- **Throughput**: Tasks claimed/sec (higher is better)
- **P99 Latency**: ≤ 100ms for claim operations
- **Redis connections**: Fewer connections when pipelining well
- **Redis commands/sec**: Should decrease with pipelining (batching effect)
