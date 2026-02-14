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

**Example**: For workloads with 90s average task duration and 1000 in-progress tasks:
````yaml
defaultLeaseSeconds: 210      # 90s * 2 + 30s
requeueInspectLimit: 100      # 10% of 1000
````

---

### Backoff and Retry Tuning

**`maxAttemptsDefault`** (default: 5)
- Tasks exceeding this limit move to DLQ
- **Lower values** (2-3): Faster failure detection, but less tolerance for transient errors
- **Higher values** (5-10): More retries, but longer time to DLQ

**`backoffPolicy`** (default: `exponential`)
- **`fixed`**: Constant delay (predictable, but may overwhelm on failure)
- **`linear`**: Gradual increase (moderate congestion control)
- **`exponential`**: Fast increase (best for transient failures)
- **`exp_full_jitter`**: Exponential + randomization (prevents thundering herd)
- **`exp_equal_jitter`**: Exponential + half jitter (balanced)

**`backoffBaseSeconds`** (default: 5)
- Initial retry delay
- **Lower values** (1-5s): Faster retries (good for transient errors)
- **Higher values** (10-30s): Reduce load on failing downstream services

**`backoffMaxSeconds`** (default: 900)
- Maximum retry delay cap
- **Recommendation**: Set based on task staleness tolerance (e.g., 15 minutes for near-real-time, 1 hour for batch)

**Example**: For batch processing with long-running tasks:
````yaml
maxAttemptsDefault: 8
backoffPolicy: exp_full_jitter
backoffBaseSeconds: 10
backoffMaxSeconds: 3600       # 1 hour max delay
````

---

### Webhook and Subscription Tuning

**`subscriptionMinIntervalSeconds`** (default: 5)
- Rate limit for worker availability notifications per subscription
- **Lower values** (1-5s): Near-instant notifications, but higher webhook traffic
- **Higher values** (10-30s): Reduced load, but slower worker awareness

**`subscriptionCleanupIntervalSeconds`** (default: 60)
- Background cleanup interval for expired subscriptions
- **Lower values** (30s): Faster cleanup, but more CPU overhead
- **Higher values** (120-300s): Reduced overhead, but stale subscriptions linger

**`resultWebhookMaxAttempts`** (default: 5)
- Task-level webhook retry count
- Set based on downstream reliability (5-10 for critical webhooks, 2-3 for best-effort)

**`resultWebhookBaseBackoffSeconds`** (default: 2)
**`resultWebhookMaxBackoffSeconds`** (default: 60)
- Retry delays for task-level webhooks
- Use aggressive retries (base: 1s, max: 30s) for low-latency webhooks
- Use conservative retries (base: 5s, max: 300s) for batch/async webhooks

**Example**: For high-frequency worker notifications:
````yaml
subscriptionMinIntervalSeconds: 2
subscriptionCleanupIntervalSeconds: 30
resultWebhookMaxAttempts: 8
resultWebhookBaseBackoffSeconds: 1
resultWebhookMaxBackoffSeconds: 30
````

---

## Redis/KVRocks Tuning

### Connection Pool

The `go-redis` client automatically manages connection pooling. For high-throughput workloads:

1. **Increase Redis max connections**:
   ````yaml
   # KVRocks config
   maxclients 10000
   ````

2. **Monitor connection exhaustion**:
   ````bash
   redis-cli -h <host> -p <port> info clients
   # Look for: connected_clients, blocked_clients
   ````

3. **Scale horizontally**: Run multiple codeQ instances (stateless service)

---

### Memory and Persistence

**KVRocks** uses RocksDB with SSD persistence. Optimize for:

1. **SSDs**: Use NVMe SSDs for low-latency delayed queue operations (ZADD, ZRANGEBYSCORE)
2. **Memory**: Allocate enough RAM for hot keys (task hashes, in-progress lists)
3. **Compaction**: Tune RocksDB compaction for write-heavy workloads (see KVRocks docs)

**Example KVRocks tuning**:
````yaml
# kvrocks.conf
max-io-mb 500                 # I/O throughput limit
compaction-checker-range 10   # Background compaction frequency
rocksdb-max-background-jobs 8 # Parallel compaction threads
````

---

## Scaling Strategies

### Horizontal Scaling (Multiple Instances)

codeQ is **stateless** and supports horizontal scaling:

1. Run multiple replicas behind a load balancer
2. Each instance can serve producers and workers independently
3. Shared KVRocks cluster handles synchronization

**Kubernetes example**:
````yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: codeq
spec:
  replicas: 5  # Scale to 5 instances
  template:
    spec:
      containers:
      - name: codeq
        image: codeq:latest
        resources:
          requests:
            cpu: "1"
            memory: "512Mi"
          limits:
            cpu: "2"
            memory: "1Gi"
````

---

### Queue Partitioning (Future)

codeQ reserves `docs/06-sharding.md` for future queue sharding. Current implementation uses a single queue per command.

For high-throughput commands, consider:
1. **Command prefixing**: Split `GENERATE_MASTER` into `GENERATE_MASTER_0`, `GENERATE_MASTER_1` (manual sharding)
2. **Worker fleet balancing**: Distribute workers evenly across command variants
3. **Priority queues**: Use `priority` field to separate urgent vs. batch tasks

---

## Monitoring and Metrics

### Queue Depth Metrics

Use admin API to monitor queue health:

````bash
curl -H "Authorization: Bearer <admin-token>" \
  https://codeq.example.com/v1/codeq/admin/queues
````

**Key metrics**:
- `ready`: Tasks available for claiming (should be low if workers are keeping up)
- `delayed`: Scheduled tasks (expected if using `runAt` or NACK backoff)
- `inProgress`: Tasks claimed by workers (should match worker fleet capacity)
- `dlq`: Failed tasks (investigate if non-zero)

**Alerting thresholds**:
- `ready > 1000` for >5 minutes → Scale up workers
- `dlq > 0` → Investigate task failures (check logs, downstream services)
- `inProgress` constantly high → Workers may be stuck (check heartbeats)

---

### Lease Expiry Repair

Monitor claim-time repair frequency:
- High repair counts indicate worker crashes or lease expirations
- Check worker logs for heartbeat failures or OOM kills

**Example log**:
````
INFO Requeued 12 expired tasks for command=GENERATE_MASTER
````

**Mitigation**:
- Increase `defaultLeaseSeconds` if tasks legitimately take longer
- Add worker heartbeat logic (POST `/tasks/:id/heartbeat` every 60s)
- Scale up worker memory/CPU if tasks are being killed

---

### Webhook Delivery

Monitor webhook failure rates:
- Check result callback retry logs for `resultWebhookMaxAttempts` exhaustion
- Check worker availability notification logs for unreachable callback URLs

**Example log**:
````
WARN Result callback failed after 5 attempts: taskId=abc-123 webhook=https://...
````

**Mitigation**:
- Increase `resultWebhookMaxAttempts` for critical webhooks
- Add webhook endpoint monitoring (uptime, response time)
- Implement webhook retry fallback (polling `GET /tasks/:id/result`)

---

## Load Testing

### Simulating Producers

Use `codeq` CLI to enqueue tasks:

````bash
for i in {1..1000}; do
  codeq task create \
    --command GENERATE_MASTER \
    --payload "{\"jobId\":\"j-$i\"}" \
    --priority $((RANDOM % 10))
done
````

### Simulating Workers

Use `test/local_flow.py` or `codeq worker start`:

````bash
# Single worker
codeq worker start --commands GENERATE_MASTER --poll-interval 1s

# Fleet (bash)
for i in {1..10}; do
  codeq worker start --commands GENERATE_MASTER --poll-interval 1s &
done
````

### Measuring Throughput

Monitor tasks/second via queue stats:

````bash
# Sample queue depth every 5s
while true; do
  curl -s -H "Authorization: Bearer <admin-token>" \
    https://codeq.example.com/v1/codeq/admin/queues/GENERATE_MASTER \
    | jq '.ready'
  sleep 5
done
````

Calculate throughput:
- **Producer throughput**: Tasks created per second (POST `/tasks`)
- **Worker throughput**: Tasks completed per second (POST `/tasks/:id/result`)
- **Queue lag**: `ready` queue depth (should be stable under load)

---

## Common Performance Issues

### Issue: High Claim Latency

**Symptoms**: `POST /tasks/claim` takes >500ms

**Causes**:
- Large `requeueInspectLimit` scanning many in-progress tasks
- Slow Redis/KVRocks (network latency, disk I/O)
- Many delayed tasks being moved to ready queue

**Solutions**:
1. Reduce `requeueInspectLimit` (e.g., 50-100)
2. Use local Redis/KVRocks (reduce network latency)
3. Add Redis connection pooling
4. Scale KVRocks vertically (faster SSD, more CPU)

---

### Issue: Tasks Stuck in `inProgress`

**Symptoms**: `inProgress` queue depth grows, workers not completing tasks

**Causes**:
- Worker crashes without abandoning tasks
- Lease expiry repair not running (no claims for that command)
- Workers stuck on long-running tasks

**Solutions**:
1. Reduce `defaultLeaseSeconds` for faster automatic requeue
2. Implement worker heartbeat (`POST /tasks/:id/heartbeat` every 60s)
3. Add worker health checks (memory, CPU, downstream service availability)
4. Force cleanup via admin API: `POST /admin/tasks/cleanup`

---

### Issue: DLQ Buildup

**Symptoms**: `dlq` queue depth grows over time

**Causes**:
- Persistent task failures (downstream service down, bad payload)
- `maxAttemptsDefault` too low for transient errors

**Solutions**:
1. Investigate DLQ tasks (retrieve via Redis: `LRANGE codeq:queue:{command}:dlq 0 -1`)
2. Fix root cause (downstream service, task payload validation)
3. Increase `maxAttemptsDefault` if failures are transient
4. Implement DLQ alerting and manual review process

---

## Further Reading

- **Configuration reference**: `docs/14-configuration.md`
- **Queue model**: `docs/05-queueing-model.md`
- **Storage layout**: `docs/07-storage-kvrocks.md`
- **Backoff policies**: `docs/11-backoff.md`
- **Webhooks**: `docs/12-webhooks.md`
- **Operations**: `docs/10-operations.md`
