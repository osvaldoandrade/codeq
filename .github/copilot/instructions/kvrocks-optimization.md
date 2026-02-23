# KVRocks Optimization Guide

## Configuration Tuning

### Block Cache (L1 + L2)

The block cache holds frequently accessed data. Tuning improves claim latency under load:

```yaml
# docker-compose.override.yml
services:
  kvrocks:
    environment:
      # Total block cache: 256MB (typical for medium deployment)
      KVROCKS_BLOCK_CACHE_SIZE: 268435456  # 256MB
      
      # For high throughput (>5k req/s), increase to 512MB-1GB
      # For low memory systems, use 64-128MB
```

Monitor with:
```bash
redis-cli -p 6379 INFO stats | grep cache
# Look for: used_block_cache_memory, block_cache_hits, block_cache_misses
```

**Rule of thumb:**
- `block_cache_hit_ratio > 90%`: Cache is effective
- Ratio < 70%: Increase cache size or profile access patterns

### Write Buffer Configuration

Affects transaction latency and throughput:

```yaml
services:
  kvrocks:
    environment:
      # Default 64MB - increase for burst loads
      KVROCKS_WRITE_BUFFER_SIZE: 104857600  # 100MB
      
      # Number of memtables to keep in memory before flushing
      KVROCKS_MAX_WRITE_BUFFERS: 3
```

### Compression

Reduces disk I/O but increases CPU:

```yaml
environment:
  # lz4 recommended for codeQ (faster than snappy, good compression)
  KVROCKS_COMPRESSION: "lz4"
  
  # Compression ratio targets
  # snappy: ~50% reduction, lowest CPU
  # lz4: ~40% reduction, medium CPU  
  # zstd: ~60% reduction, high CPU
```

## Connection Pooling

codeQ uses go-redis with connection pool. Tune for workload:

```go
// In pkg/config/config.go or environment
REDIS_POOL_SIZE=100  // Default: 10, increase for concurrent workers
```

Monitor connection usage:
```bash
redis-cli -p 6379 INFO clients | grep connected_clients
# Should be: ~= POOL_SIZE * num_request_handlers
```

## Profiling Commands

Identify slow operations:

```bash
# Monitor command latencies
redis-cli --latency
# Interval: 100ms
# Output shows p50, p99 latencies

# Slow log (ops >10ms)
redis-cli slowlog get 10
redis-cli slowlog len

# Memory usage by key pattern
redis-cli --bigkeys -i 0.01
```

## Claim Path Optimization

The claim operation (`BRPOPLPUSH` + JSON update) is critical. Monitor:

```bash
# Check Redis command breakdown
redis-cli INFO commandstats | grep "cmdstat_brpop\|cmdstat_lpush"

# Expected for healthy claim:
# - 1 BRPOPLPUSH (blocking pop-and-push)
# - 1 EVAL (Lua script for atomic JSON update)
# - 1 EXPIRE (set lease TTL)
```

If latency is high:
1. Check block_cache_hit_ratio
2. Monitor slow log for JSON parsing bottlenecks
3. Consider batch claim operations (claim multiple tasks at once)

## Memory Tuning

Set max memory to prevent OOM:

```yaml
environment:
  # Leave 1-2GB for system, rest for KVRocks
  # Production: Usually 16-32GB instances
  KVROCKS_MAXMEMORY: 8589934592  # 8GB
  
  # Eviction policy when maxmemory reached
  # allkeys-lru: Evict least recently used (safe for task queues)
  KVROCKS_MAXMEMORY_POLICY: "allkeys-lru"
```

## Scaling to High Throughput (10k+ req/s)

Recommended settings for production:

```yaml
services:
  kvrocks:
    environment:
      KVROCKS_BLOCK_CACHE_SIZE: 1073741824  # 1GB
      KVROCKS_WRITE_BUFFER_SIZE: 209715200  # 200MB
      KVROCKS_MAX_WRITE_BUFFERS: 4
      KVROCKS_COMPRESSION: "lz4"
      KVROCKS_MAXMEMORY: 17179869184  # 16GB (reserve 16GB for codeQ + OS)
    resources:
      limits:
        cpus: "8"
        memory: 32GB
```

## Monitoring Checklist

- [ ] Block cache hit ratio > 90%
- [ ] p99 Redis command latency < 20ms
- [ ] Connected clients approaching but below pool size
- [ ] Memory usage stable (not growing unbounded)
- [ ] CPU usage <60% at peak load (headroom for spikes)
