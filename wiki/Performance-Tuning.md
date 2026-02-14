# Performance Tuning

This guide covers performance optimization strategies for codeQ deployments.

## Configuration for High Throughput

### Lease and Timeout Tuning

**`defaultLeaseSeconds`** (default: 300)
- Controls how long a worker holds a task before automatic requeue
- **Lower values** (60-120s): Faster recovery from worker crashes, but more heartbeat overhead
- **Higher values** (300-600s): Reduce heartbeat traffic, but slower failure detection
- **Recommendation**: Set to 2× typical task duration + 30s safety margin

**`requeueInspectLimit`** (default: 200)
- Maximum tasks to scan for expired leases during claim-time repair
- **Lower values** (50-100): Faster claim operations, but delayed lease expiry repair
- **Higher values** (200-500): More thorough repair, but slower claim when many tasks are in-progress
- **Recommendation**: Set to 10% of expected `inProgress` queue depth

### Backoff and Retry Tuning

**`maxAttemptsDefault`** (default: 5)
- Tasks exceeding this limit move to DLQ
- Balance between retry tolerance and failure detection speed

**`backoffPolicy`** options:
- **`fixed`**: Constant delay
- **`linear`**: Gradual increase
- **`exponential`**: Fast increase (best for transient failures)
- **`exp_full_jitter`**: Exponential + randomization (prevents thundering herd)
- **`exp_equal_jitter`**: Exponential + half jitter (balanced)

**`backoffBaseSeconds`** and **`backoffMaxSeconds`**: Control retry timing

### Webhook and Subscription Tuning

**`subscriptionMinIntervalSeconds`** (default: 5)
- Rate limit for worker availability notifications
- Lower values = faster notifications, higher webhook traffic

**`subscriptionCleanupIntervalSeconds`** (default: 60)
- Background cleanup interval for expired subscriptions

**`resultWebhookMaxAttempts`** (default: 5)
- Task-level webhook retry count
- Set based on downstream reliability

## Redis/KVRocks Tuning

### Connection Pool

The `go-redis` client automatically manages connection pooling. For high-throughput workloads:

1. Increase Redis max connections
2. Monitor connection exhaustion
3. Scale horizontally with multiple codeQ instances (stateless service)

### Memory and Persistence

**KVRocks** uses RocksDB with SSD persistence. Optimize for:

1. **SSDs**: Use NVMe SSDs for low-latency operations
2. **Memory**: Allocate enough RAM for hot keys
3. **Compaction**: Tune RocksDB compaction for write-heavy workloads

## Scaling Strategies

### Horizontal Scaling

codeQ is **stateless** and supports horizontal scaling:

1. Run multiple replicas behind a load balancer
2. Each instance serves producers and workers independently
3. Shared KVRocks cluster handles synchronization

### Queue Partitioning

For high-throughput commands, consider:
1. **Command prefixing**: Manual sharding via command variants
2. **Worker fleet balancing**: Distribute workers evenly
3. **Priority queues**: Use `priority` field to separate urgent vs. batch tasks

## Monitoring and Metrics

### Queue Depth Metrics

Use admin API to monitor queue health. Key metrics:
- `ready`: Tasks available for claiming
- `delayed`: Scheduled tasks
- `inProgress`: Tasks claimed by workers
- `dlq`: Failed tasks (investigate if non-zero)

### Alerting Thresholds

- `ready > 1000` for >5 minutes → Scale up workers
- `dlq > 0` → Investigate task failures
- `inProgress` constantly high → Workers may be stuck

### Lease Expiry Repair

Monitor claim-time repair frequency. High repair counts indicate worker crashes or lease expirations.

### Webhook Delivery

Monitor webhook failure rates and retry exhaustion.

## Load Testing

### Simulating Producers

Use `codeq` CLI to enqueue tasks in bulk.

### Simulating Workers

Use `codeq worker start` or test scripts to simulate worker fleets.

### Measuring Throughput

Monitor tasks/second via queue stats API. Calculate:
- **Producer throughput**: Tasks created per second
- **Worker throughput**: Tasks completed per second
- **Queue lag**: `ready` queue depth stability

## Common Performance Issues

### High Claim Latency

**Causes**: Large `requeueInspectLimit`, slow Redis, many delayed tasks

**Solutions**: Reduce `requeueInspectLimit`, use local Redis, scale KVRocks

### Tasks Stuck in `inProgress`

**Causes**: Worker crashes, no lease expiry repair, stuck workers

**Solutions**: Reduce `defaultLeaseSeconds`, implement heartbeat, add health checks

### DLQ Buildup

**Causes**: Persistent failures, low `maxAttemptsDefault`

**Solutions**: Investigate DLQ tasks, fix root cause, increase retry limit

## Further Reading

- [Configuration](Configuration)
- [Queueing Model](Queueing-Model)
- [Storage (KVRocks)](Storage-KVRocks)
- [Retry and Backoff](Retry-and-Backoff)
- [Operations](Operations)
