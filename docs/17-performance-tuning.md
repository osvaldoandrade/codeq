# Performance Tuning

This guide provides comprehensive tuning recommendations for production codeQ deployments. Follow these guidelines to optimize throughput, reduce latency, and ensure stable operations under load.

## 1) KVRocks Configuration

KVRocks is the stateful storage component and often the primary bottleneck. Proper configuration is critical for performance.

### Memory allocation

KVRocks uses RocksDB under the hood, which relies on memory for caching and buffering.

**Recommended settings:**

```conf
# kvrocks.conf
maxmemory 8gb
# Set to 70-80% of available RAM, leaving room for OS and KVRocks overhead
```

**Block cache sizing:**

- Default block cache: 2GB per instance
- For read-heavy workloads: increase to 30-50% of `maxmemory`
- For write-heavy workloads: increase write buffer size

```conf
rocksdb.block_cache_size 4096
# In MB. Adjust based on read vs write ratio.
```

**Memory considerations:**

- Monitor actual memory usage with `INFO memory`
- Reserve 20-30% of system RAM for OS page cache
- Avoid swapping at all costs; set `vm.swappiness=1` on Linux hosts

### Persistence settings

KVRocks persists using RocksDB SST files. Tune based on durability vs performance tradeoff.

**Write-ahead log (WAL):**

```conf
rocksdb.wal_ttl_seconds 0
rocksdb.wal_size_limit_mb 0
# Disable WAL limits for max durability
```

For higher throughput with reduced durability guarantees:

```conf
rocksdb.wal_ttl_seconds 3600
rocksdb.wal_size_limit_mb 2048
```

**Compaction:**

- Background compactions reduce read amplification but use CPU
- Default settings are usually sufficient
- Monitor compaction stats: `rocksdb.compaction.stats`

```conf
rocksdb.max_background_compactions 4
rocksdb.max_background_flushes 2
```

**Snapshot intervals:**

- KVRocks creates periodic snapshots
- Balance backup frequency with I/O overhead
- Use external backups instead of frequent snapshots

### Connection pooling

codeQ uses `go-redis` which manages connections automatically. Tune pool size based on API server concurrency.

**Recommended pool size:**

- Default: 10 connections per API server instance
- High throughput: `poolSize = 2 * (API_WORKERS + background_jobs)`

In Go code or via environment:

```go
redis.Options{
    PoolSize: 20,
    MinIdleConns: 5,
    MaxRetries: 3,
    PoolTimeout: 4 * time.Second,
}
```

**Connection limits:**

KVRocks default: `maxclients 10000`

For large deployments:

```conf
maxclients 20000
tcp-backlog 511
# Ensure OS socket backlog is also increased
```

### Cluster configuration

**Current state:** codeQ does not implement sharding. All data resides on a single KVRocks instance or cluster node.

**Future sharding (not yet implemented):**

When sharding is introduced:

- Use consistent hashing over command types
- Shard by `command` key segment
- Client-side routing or Redis Cluster protocol

For now, scale KVRocks vertically. For horizontal scaling, deploy multiple isolated codeQ+KVRocks pairs per region or tenant.

**Replication (KVRocks native):**

KVRocks supports master-replica replication:

```conf
slaveof <master-ip> <master-port>
# On replica nodes
```

- Use replicas for read-only queries (not currently supported by codeQ)
- Fail-over requires manual intervention or external orchestration (e.g., Sentinel)

## 2) codeQ Configuration

Tune codeQ service parameters based on workload characteristics and worker capacity.

### Worker concurrency

Control how many tasks workers can claim and process in parallel.

**Lease duration:**

- `defaultLeaseSeconds` (default: 300)
- Shorter leases (60-120s): faster retry on worker failure, higher requeue overhead
- Longer leases (300-600s): slower retry, reduced claim-time repair cost

**Decision tree:**

```
Task duration < 30s → leaseSeconds = 60
Task duration 30s-2min → leaseSeconds = 120
Task duration 2-5min → leaseSeconds = 300
Task duration > 5min → leaseSeconds = 600
```

Workers can override per-claim:

```json
{"commands":["RENDER_VIDEO"],"leaseSeconds":600}
```

**Requeue inspection limit:**

- `requeueInspectLimit` (default: 200)
- Limits how many in-progress tasks are scanned during claim-time repair
- Higher values: more thorough repair, increased claim latency
- Lower values: faster claims, potential for orphaned tasks

**Performance note**: Since v1.1.0, the in-progress queue uses a SET data structure with pipelined TTL checks, 
making repair significantly faster. The O(1) `SREM` operation (vs previous O(N) `LREM`) allows higher 
`requeueInspectLimit` values without proportional latency increase. Typical claim overhead is now <5ms 
even with 500+ in-progress tasks.

Recommended:

- Low throughput (< 100 tasks/min): 500
- Medium throughput (100-1000 tasks/min): 200
- High throughput (> 1000 tasks/min): 50

### Batch sizes

codeQ does not batch enqueue or claim operations at the API level. Each operation handles one task.

For bulk operations, clients should:

- Use parallel requests with connection pooling
- Implement client-side batching for idempotency key management
- Avoid tight loops; use worker pools to parallelize enqueue

### Webhook settings

Webhooks can become a bottleneck if misconfigured.

**Result callback retry:**

- `resultWebhookMaxAttempts` (default: 5)
- `resultWebhookBaseBackoffSeconds` (default: 2)
- `resultWebhookMaxBackoffSeconds` (default: 60)

For critical callbacks, increase retries:

```yaml
resultWebhookMaxAttempts: 10
resultWebhookMaxBackoffSeconds: 300
```

For best-effort callbacks, reduce retries to save resources:

```yaml
resultWebhookMaxAttempts: 3
resultWebhookMaxBackoffSeconds: 30
```

**Worker notification rate limiting:**

- `subscriptionMinIntervalSeconds` (default: 5)
- Prevents notification storms when many tasks arrive rapidly
- Increase for bursty workloads: 10-30 seconds
- Decrease for latency-sensitive workloads: 1-2 seconds

**Subscription cleanup:**

- `subscriptionCleanupIntervalSeconds` (default: 60)
- Removes expired subscriptions
- Not performance-critical; default is fine for most use cases

### Max attempts and backoff

Retry behavior impacts both throughput and latency.

**Max attempts:**

- `maxAttemptsDefault` (default: 5)
- Tasks exceeding this move to DLQ
- Increase for transient failures (network, rate limits): 10
- Decrease for fast-fail: 3

**Backoff policy:**

- `backoffPolicy`: `fixed|linear|exponential|exp_full_jitter|exp_equal_jitter`
- `backoffBaseSeconds` (default: 5)
- `backoffMaxSeconds` (default: 900)

**Comparison:**

| Policy | Use Case | Pros | Cons |
|--------|----------|------|------|
| `fixed` | Debugging, predictable retry | Simple | No exponential backoff |
| `linear` | Gradual retry | Moderate backoff | Can still overwhelm |
| `exponential` | Standard production | Reduces load on retry | Thundering herd |
| `exp_full_jitter` | High concurrency | Spreads retry times | Less predictable |
| `exp_equal_jitter` | Balanced | Good spread, some predictability | Slightly more complex |

**Recommended:**

```yaml
backoffPolicy: exp_full_jitter
backoffBaseSeconds: 10
backoffMaxSeconds: 900
maxAttemptsDefault: 5
```

## 3) Scaling Strategies

### Horizontal scaling (API servers)

codeQ API servers are stateless and can scale horizontally without coordination.

**Kubernetes HPA:**

```yaml
autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
```

**Metrics for scaling:**

- CPU utilization: 60-80% target
- Request rate: scale when approaching connection limits
- Response time: scale if p99 latency exceeds 200ms

**Load balancing:**

- Use Kubernetes ClusterIP service with round-robin
- For sticky sessions (not required for codeQ): consider IP hash

**Connection pooling:**

- Each API instance maintains a Redis connection pool
- Total connections = `replicas * poolSize`
- Ensure KVRocks `maxclients` > total connections + headroom

Example for 10 replicas with poolSize=20:
```conf
maxclients 250
# 10 * 20 = 200 + 50 headroom
```

### Vertical scaling (KVRocks)

KVRocks is CPU and memory intensive. Scale vertically before attempting horizontal sharding.

**CPU:**

- Minimum: 2 vCPUs
- Recommended: 4-8 vCPUs
- High throughput: 16+ vCPUs

**Memory:**

- Minimum: 4 GB
- Recommended: 8-16 GB
- High volume: 32-64 GB

**Disk:**

- Use SSDs for persistent storage
- Provision 3-5x expected data size for compaction overhead
- Monitor disk I/O: aim for < 70% utilization

**Resource allocation (Kubernetes):**

```yaml
kvrocks:
  resources:
    requests:
      cpu: 4000m
      memory: 16Gi
    limits:
      cpu: 8000m
      memory: 16Gi
```

### Sharding considerations

**Current limitation:** Sharding is not implemented. See `docs/06-sharding.md`.

**When sharding becomes necessary:**

- Single KVRocks instance saturates CPU (> 80%)
- Memory requirements exceed 64 GB
- Network bandwidth bottleneck (> 1 Gbps sustained)

**Future sharding strategy:**

- Shard by `command` type
- Use consistent hashing for balanced distribution
- Implement client-side routing or Redis Cluster mode
- Monitor shard distribution and rebalance as needed

**Workarounds without sharding:**

- Deploy independent codeQ+KVRocks pairs per region/tenant
- Partition workloads by command type manually
- Use queue depth monitoring to detect imbalance

### Multi-region deployments

For global workloads, deploy codeQ+KVRocks pairs per region.

**Architecture:**

```
Region A: API Servers → KVRocks A
Region B: API Servers → KVRocks B
Region C: API Servers → KVRocks C
```

**Benefits:**

- Reduced latency for local workers and producers
- Fault isolation
- Independent scaling per region

**Considerations:**

- No cross-region queue visibility
- Producers and workers must target the correct region
- Use DNS or service mesh for region-aware routing

**Multi-region task distribution:**

For workloads requiring global distribution:

- Use a coordinator service to distribute tasks across regions
- Producers enqueue to local region, workers claim from local region
- For cross-region tasks, use webhooks to trigger secondary enqueue

## 4) Performance Benchmarks

Performance varies based on workload, infrastructure, and configuration. These benchmarks provide baseline expectations.

### Test environment

- KVRocks: 4 vCPUs, 8 GB RAM, SSD storage
- codeQ API: 4 instances, 2 vCPUs each, poolSize=10
- Network: internal cluster network, < 1ms RTT

### Throughput vs latency tradeoffs

**Enqueue throughput:**

| Concurrency | Throughput (tasks/sec) | p50 Latency | p99 Latency |
|-------------|------------------------|-------------|-------------|
| 10 | 500 | 8ms | 25ms |
| 50 | 2,000 | 15ms | 60ms |
| 100 | 3,500 | 25ms | 120ms |
| 200 | 4,200 | 40ms | 250ms |

**Observations:**

- Linear scaling up to ~100 concurrent clients
- Latency degrades beyond 3,500 tasks/sec
- Bottleneck: KVRocks CPU at ~80%

**Claim throughput:**

| Concurrency | Throughput (claims/sec) | p50 Latency | p99 Latency |
|-------------|-------------------------|-------------|-------------|
| 10 | 400 | 10ms | 30ms |
| 50 | 1,500 | 20ms | 80ms |
| 100 | 2,200 | 35ms | 150ms |
| 200 | 2,500 | 60ms | 300ms |

**Observations:**

- Claim operations are more expensive due to atomic queue moves (Lua `RPOP` + `SADD`) + lease creation
- Claim-time repair adds latency when in-progress is large (bounded scan with pipelined `TTL` checks)
- Since v1.1.0: O(1) SET removal (`SREM`) instead of O(N) LIST removal (`LREM`) significantly improves repair performance
- Pipelined TTL checks reduce repair from O(L × RTT) to O(1 × RTT) where L is scan limit and RTT is network round-trip time
- Under typical production loads (100-500 in-progress tasks), claim latency improved by ~40% post-optimization
- Reduce `requeueInspectLimit` to improve claim latency under load, but modern SET-based implementation handles higher limits efficiently

**Result submission throughput:**

| Concurrency | Throughput (results/sec) | p50 Latency | p99 Latency |
|-------------|--------------------------|-------------|-------------|
| 10 | 600 | 7ms | 20ms |
| 50 | 2,500 | 12ms | 50ms |
| 100 | 4,000 | 20ms | 100ms |
| 200 | 4,500 | 35ms | 200ms |

**Observations:**

- Result submission is lightweight (hash write + list remove)
- Webhooks (if configured) add latency but run asynchronously

### Queue depth impact

Queue depth affects claim-time repair cost.

| Pending | In-progress | Claim Latency |
|---------|-------------|---------------|
| 100 | 10 | 8ms |
| 1,000 | 50 | 12ms |
| 10,000 | 100 | 15ms |
| 100,000 | 500 | 50ms |

**Recommendations:**

- Keep in-progress queue small (< 1,000 tasks)
- Use appropriate lease durations to minimize stale leases
- Monitor queue depths and scale workers to drain pending queues

### Priority queue performance

Priority queues add minimal overhead.

| Priority Levels | Claim Latency Overhead |
|-----------------|------------------------|
| 1 (no priority) | baseline |
| 3 | +0.5ms |
| 5 | +1ms |
| 10 | +2ms |

**Observations:**

- Claim checks higher priority lists first (O(1) per list)
- Negligible impact for < 10 priority levels
- Avoid priority inversion: don't starve low-priority queues

### Webhook overhead

Webhook delivery runs asynchronously but consumes resources.

**Result callbacks:**

| Webhook Latency | Task Completion Latency Overhead |
|-----------------|----------------------------------|
| 50ms | +2ms (async) |
| 200ms | +2ms (async) |
| 1000ms | +2ms (async) |

**Observations:**

- Webhook dispatch is non-blocking
- Retries consume background goroutines and memory
- Failed webhooks can accumulate; monitor retry queue depth

**Worker notifications:**

- Fanout mode: O(n) where n = active subscriptions
- Group mode: O(1) per group
- Hash mode: O(1)

For large worker fleets (> 100 instances), use `group` or `hash` delivery mode.

## 5) Monitoring & Troubleshooting

### Key metrics to watch

**KVRocks metrics:**

- `used_memory`: Current memory usage
- `connected_clients`: Active connections
- `instantaneous_ops_per_sec`: Operations per second
- `keyspace`: Total keys (should stabilize after initial ramp-up)

**codeQ application metrics:**

codeQ exposes Prometheus metrics at `GET /metrics` (see `docs/10-operations.md`). Key metrics to watch:

- Request rate (enqueue, claim, complete) per command
- Request latency (p50, p95, p99)
- Queue depths (pending, in-progress, delayed, DLQ) per command
- Active leases
- Webhook delivery success/failure rates
- Subscription count

**Recommended tooling:**

- Prometheus + Grafana for metrics
- Structured logs with `logFormat=json` for centralized logging
- Distributed tracing (OpenTelemetry) for request correlation

### Common bottlenecks

#### 1. KVRocks CPU saturation

**Symptoms:**

- High latency across all operations
- `instantaneous_ops_per_sec` plateaus
- CPU usage > 80%

**Solutions:**

- Vertical scale: increase vCPUs
- Optimize claim-time repair: reduce `requeueInspectLimit`
- Reduce connection pool size to limit concurrent operations
- Consider read replicas for stats queries (future enhancement)

#### 2. Memory exhaustion

**Symptoms:**

- `used_memory` approaching `maxmemory`
- Eviction warnings in KVRocks logs
- Slow queries due to disk I/O

**Solutions:**

- Increase `maxmemory` and host RAM
- Reduce task retention window (default: 24 hours)
- Run cleanup more frequently
- Archive completed tasks to external storage

#### 3. Large in-progress queue

**Symptoms:**

- Slow claim operations (> 100ms p99)
- Tasks not requeuing on lease expiry
- Workers reporting timeouts

**Solutions:**

- Reduce `leaseSeconds` to accelerate requeue
- Increase `requeueInspectLimit` temporarily (trade latency for correctness)
- Scale workers to drain in-progress tasks
- Investigate worker failures or slow task processing

#### 4. Webhook delivery failures

**Symptoms:**

- Increasing retry queue depth
- High memory usage from pending webhooks
- Delayed task completion acknowledgment

**Solutions:**

- Reduce `resultWebhookMaxAttempts`
- Move to pull-based polling for result retrieval
- Validate webhook endpoint health and latency
- Use webhook failure DLQ (not implemented; requires custom solution)

#### 5. Network latency

**Symptoms:**

- High p99 latency even under low load
- Timeouts between API servers and KVRocks

**Solutions:**

- Deploy KVRocks in the same region/AZ as API servers
- Use dedicated network for backend traffic
- Monitor network interface saturation
- Compress large payloads (not supported; requires custom encoding)

### Debugging slow operations

**Enable detailed logging:**

```yaml
logLevel: debug
logFormat: json
```

**Identify slow commands in KVRocks:**

```bash
redis-cli --latency -h <kvrocks-host> -p 6666
redis-cli --latency-history -h <kvrocks-host> -p 6666
```

**Trace request flow:**

- Use correlation IDs in request headers
- Log task ID, worker ID, and command type
- Correlate API logs with KVRocks command logs

**Profile KVRocks:**

```bash
redis-cli -h <kvrocks-host> -p 6666 --stat
# Shows real-time stats on keys, memory, and commands
```

**Profile Go service:**

Enable pprof endpoints (requires code change):

```go
import _ "net/http/pprof"
go http.ListenAndServe(":6060", nil)
```

Analyze CPU and memory profiles:

```bash
go tool pprof http://localhost:6060/debug/pprof/profile
go tool pprof http://localhost:6060/debug/pprof/heap
```

### Capacity planning

**Estimate baseline load:**

1. Expected task creation rate (tasks/min)
2. Average task duration (seconds)
3. Peak concurrency factor (e.g., 3x during business hours)

**Example calculation:**

- 1,000 tasks/min average
- 120s average duration
- 3x peak factor

**Peak load:**

- Enqueue: 3,000 tasks/min = 50 tasks/sec
- In-progress: 3,000 tasks/min * 2 min = 6,000 concurrent tasks
- Claim rate: 50 tasks/sec

**Resource requirements:**

- KVRocks: 4-8 vCPUs, 16 GB RAM (from benchmarks, 50 tasks/sec is well within capacity)
- API servers: 3-5 replicas (from benchmarks, 50 tasks/sec = low load)
- Workers: 6,000 / (tasks per worker) = worker fleet size

**Growth planning:**

- 2x load: increase KVRocks to 8 vCPUs, scale API to 5-10 replicas
- 5x load: consider sharding or multi-region deployment
- 10x load: implement sharding, use dedicated KVRocks cluster

**Monitoring thresholds:**

| Metric | Warning | Critical |
|--------|---------|----------|
| KVRocks CPU | 70% | 85% |
| KVRocks Memory | 75% | 90% |
| API p99 Latency | 150ms | 500ms |
| Pending Queue Depth | 10,000 | 50,000 |
| In-progress Queue Depth | 1,000 | 5,000 |
| DLQ Depth | 100 | 1,000 |

**Action items:**

- Warning: Investigate and plan capacity increase
- Critical: Scale immediately or throttle producers

## Configuration Examples

### Low-latency setup

For latency-sensitive workloads (< 50ms p95):

```yaml
config:
  defaultLeaseSeconds: 60
  requeueInspectLimit: 50
  backoffPolicy: fixed
  backoffBaseSeconds: 2
  subscriptionMinIntervalSeconds: 1

kvrocks:
  resources:
    requests:
      cpu: 4000m
      memory: 8Gi
    limits:
      cpu: 8000m
      memory: 8Gi
```

### High-throughput setup

For high task volume (> 5,000 tasks/min):

```yaml
config:
  defaultLeaseSeconds: 300
  requeueInspectLimit: 200
  backoffPolicy: exp_full_jitter
  backoffBaseSeconds: 10
  backoffMaxSeconds: 900

autoscaling:
  enabled: true
  minReplicas: 5
  maxReplicas: 20
  targetCPUUtilizationPercentage: 70

kvrocks:
  resources:
    requests:
      cpu: 8000m
      memory: 32Gi
    limits:
      cpu: 16000m
      memory: 32Gi
```

### Multi-region setup

For global distribution:

```yaml
# Region A
config:
  redisAddr: kvrocks-us-east:6666
  env: prod-us-east

# Region B
config:
  redisAddr: kvrocks-eu-west:6666
  env: prod-eu-west

# Use DNS or service mesh routing to direct clients to nearest region
```

## 10) Metrics and Monitoring Performance

Prometheus metrics provide observability but have performance implications that should be understood.

### Metrics Scrape Overhead

The `/metrics` endpoint is unauthenticated and performs Redis queries on each scrape via the custom `redisCollector`.

**Scrape behavior:**
- Each scrape triggers a Redis pipeline with ~20-30 commands (queue depths for all commands × priority buckets)
- Scrape timeout: 2 seconds (hardcoded in `redis_collector.go`)
- Default Prometheus scrape interval: 15 seconds

**Cost analysis:**
````
Single scrape overhead:
- Redis commands: ~25 LLEN + ZCARD operations
- Latency: 1-10ms (depends on KVRocks load and network)
- CPU: Negligible (all work done in Redis)

Per-replica load (15s scrape interval):
- 4 scrapes/minute × 25 commands = 100 Redis ops/minute per replica
- With 10 API replicas: 1000 Redis ops/minute = ~17 ops/sec
````

**Scaling considerations:**

1. **Many replicas**: Queue depth queries are replicated across all API servers
   - With 100 replicas: 1700 Redis ops/sec just for metrics
   - Solution: Reduce scrape frequency or use service-level monitoring (scrape 1 replica)

2. **PromQL deduplication**: All replicas report identical queue depths (Redis is source of truth)
   - Always use `max by (command, queue)` in dashboards
   - Example: `max by (command, queue) (codeq_queue_depth)`

3. **Scrape timeout failures**: If Redis is slow, scrapes may timeout
   - Monitor Prometheus scrape success rate: `up{job="codeq"}`
   - Increase KVRocks resources if scrapes fail frequently
   - Consider increasing scrape interval from 15s to 30s or 60s

### Reducing Metrics Overhead

**Reduce scrape frequency:**
````yaml
# prometheus.yml
scrape_configs:
  - job_name: codeq
    scrape_interval: 60s  # Default: 15s
    scrape_timeout: 10s   # Default: 10s
````

**Service-level monitoring** (scrape only one replica):
````yaml
# prometheus.yml
scrape_configs:
  - job_name: codeq
    static_configs:
      - targets: ['codeq-0.codeq.svc.cluster.local:8080']  # Single pod
````

**Sidecar pattern** (separate metrics endpoint):
- Run a dedicated metrics exporter service that queries Redis
- API servers only expose application-level counters/histograms
- Reduces load on production API servers

### Metric Cardinality

High-cardinality labels can degrade Prometheus performance.

**Current labels (safe):**
- `command`: 2 values (CmdGenerateMaster, CmdGenerateCreative)
- `queue`: 4 values (ready, delayed, in_progress, dlq)
- `status`: 2 values (COMPLETED, FAILED)
- `outcome`: 2 values (success, failure)
- Total cardinality: < 100 unique time series

**Avoid high-cardinality labels:**
- ❌ Task IDs (unbounded)
- ❌ Worker IDs (scales with fleet size)
- ❌ Webhook URLs (potentially thousands)
- ❌ User IDs (unbounded)

**See**: `docs/18-developer-guide.md#adding-metrics` for best practices

### Monitoring Best Practices

**Essential metrics to monitor:**
- Task throughput: `rate(codeq_task_created_total[5m])`
- Queue depth: `max by (command, queue) (codeq_queue_depth)`
- Completion rate: `rate(codeq_task_completed_total[5m])`
- DLQ growth: `delta(codeq_dlq_depth[5m])`
- P95 latency: `histogram_quantile(0.95, rate(codeq_task_processing_latency_seconds_bucket[5m]))`

**Alerting thresholds:**
- DLQ depth increasing: Sign of persistent failures
- Queue depth growing faster than completion rate: System overload
- Lease expiry rate spiking: Workers crashing or hanging
- Webhook failure rate > 5%: Downstream service issues

**Grafana dashboard:**
- Import `docs/grafana/codeq-dashboard.json`
- Includes pre-configured panels for all key metrics
- Uses multi-replica safe queries with `max by (...)`

## Further Reading

- Configuration reference: `docs/14-configuration.md`
- KVRocks storage layout: `docs/07-storage-kvrocks.md`
- Architecture overview: `docs/03-architecture.md`
- Queueing model: `docs/05-queueing-model.md`
- Webhooks: `docs/12-webhooks.md`
- Backoff policies: `docs/11-backoff.md`
- Operations: `docs/10-operations.md`
- Sharding (future): `docs/06-sharding.md`
- Helm chart: `helm/codeq/values.yaml`

## Summary

Performance tuning requires balancing throughput, latency, and resource costs. Start with recommended defaults, monitor key metrics, and adjust based on observed bottlenecks. Scale API servers horizontally and KVRocks vertically. Use decision trees to select appropriate lease durations, backoff policies, and webhook configurations. For extreme scale, plan for sharding or multi-region deployments.
