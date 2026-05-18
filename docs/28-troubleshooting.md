# Troubleshooting

This guide covers common issues, their symptoms, and resolution steps. For metric definitions and PromQL examples see [Operations](10-operations.md). For alerting rule configuration see `deploy/docker-compose/local-dev/alerting-rules.yml`.

## Quick diagnostics

| Check | Command / URL | Healthy response |
|---|---|---|
| API health | `GET /healthz` | `{"status":"ok"}` |
| Prometheus scrape | `GET /metrics` | Prometheus text format, no errors |
| Pebble data directory | `ls -la <pebbleDir>/LOCK` | File exists, owned by the codeq process |

## Common issues

### 1. API fails to start with "open pebble: resource temporarily unavailable" or lock errors

**Symptoms:** API startup fails with `open pebble: ... lock`, `cannot acquire LOCK`, or `resource temporarily unavailable` referencing the configured `pebbleDir`.

**Possible causes:**
- Another codeq process is already running against the same `pebbleDir` (Pebble enforces an exclusive lock via `<pebbleDir>/LOCK`).
- A previous codeq process crashed without releasing the lock and `LOCK` is stale (rare; Pebble normally recovers).
- The data directory is on a read-only mount or the process lacks write permission.
- The data directory lives on a remote filesystem (NFS, SMB) that does not honor `flock`/`fcntl` locks — unsupported.

**Resolution:**
1. Run `lsof <pebbleDir>/LOCK` (or `fuser`) to confirm no other process is holding the lock.
2. If no process is attached and the file is stale, remove `<pebbleDir>/LOCK` and restart codeq.
3. Verify the codeq UID has read/write on `pebbleDir` and that the mount is not read-only (`mount | grep <pebbleDir>`).
4. Move `pebbleDir` to a local disk if it is on NFS/SMB — Pebble requires POSIX-compliant local storage.
5. Review the full startup log: Pebble surfaces the exact `os.OpenFile` error.

### 2. API returns `503` or does not start

**Symptoms:** `/healthz` is unreachable or returns an error.

**Possible causes:**
- `pebbleDir` is unreachable or the lock cannot be acquired (see issue 1).
- Port conflict on the configured listen port.
- Invalid YAML configuration file.

**Resolution:**
1. Verify `pebbleDir` exists, is writable, and is not locked by another process.
2. Check the codeq log output for startup errors.
3. Validate `config.yml` syntax with a YAML linter.
4. Ensure the listen port is not already in use.

### 3. Tasks stuck in ready queue (not being claimed)

**Symptoms:** `codeq_queue_depth{queue="ready"}` grows while `codeq_task_claimed_total` rate is zero.

**Possible causes:**
- No workers are running or connected.
- Workers are polling for a different `command` value.
- Network partition between workers and the API.

**Resolution:**
1. Confirm at least one worker is polling `POST /v1/codeq/tasks/claim` with the correct `command`.
2. Check worker logs for connection errors.
3. Verify network connectivity between worker and API.

### 4. Growing dead-letter queue (DLQ)

**Symptoms:** `codeq_dlq_depth` increases steadily.

**Possible causes:**
- Tasks fail repeatedly and exhaust retry attempts.
- Worker bugs cause consistent processing failures.
- Malformed task payloads.

**Resolution:**
1. Inspect DLQ tasks via `GET /v1/codeq/admin/tasks?status=FAILED`.
2. Check the `resultCode` and `resultPayload` for error details.
3. Review worker logs at the time tasks failed.
4. After fixing the root cause, requeue tasks from the DLQ.

### 5. High end-to-end latency

**Symptoms:** `histogram_quantile(0.95, ...)` on `codeq_task_processing_latency_seconds` is above expected SLO.

**Possible causes:**
- Pebble compaction backlog (write amplification spiking L0 → L6).
- Disk pressure: `pebbleDir` near full, or the underlying device saturated on IOPS.
- Too few shards for the producer rate (`numShards` too low — contention on a single Pebble instance).
- Too few workers for the queue throughput.
- Large task payloads increasing batch commit time.
- `fsyncOnCommit: true` without the budget for the latency cost (~20% throughput hit in our harness).

**Resolution:**
1. Check Pebble metrics: `codeq_pebble_l0_files`, `codeq_pebble_compaction_bytes_in_flight`, `codeq_pebble_wal_size_bytes`. Sustained L0 file count above ~20 indicates compaction is falling behind.
2. Check disk utilization on `pebbleDir` (`df`, `iostat -x 1`). If IOPS-bound, move to faster storage or reduce write rate.
3. Increase `numShards` to spread writes across independent Pebble instances (see [Performance tuning](./17-performance-tuning.md)). Note: cluster mode and `numShards > 1` are mutually exclusive.
4. Scale workers horizontally.
5. Disable `fsyncOnCommit` if it is on and the durability budget allows (default is off).
6. Consider reducing `artifactIn` payload size.

### 6. Lease expirations spiking

**Symptoms:** `codeq_lease_expired_total` rate increases.

**Possible causes:**
- Workers crash or become unresponsive during task processing.
- Lease duration (`leaseTimeout`) is too short for the workload.
- Resource exhaustion on worker hosts (CPU, memory, file descriptors).

**Resolution:**
1. Increase `leaseTimeout` in configuration if tasks legitimately take longer.
2. Add liveness checks and resource monitoring to workers.
3. Check worker logs for panics or OOM kills.
4. Review task processing duration to set an appropriate lease value.

### 7. Cluster bloom gossip lag (cross-node ID lookups miss)

> Note: bloom gossip is a cluster-mode (Phase 5) optimization. If you're
> running RAFT ([40-raft-replication.md](40-raft-replication.md)), this
> section doesn't apply.

**Symptoms:** In cluster mode, `GetResult` or `GetTask` by ID intermittently returns `not found` for tasks that exist on another node; the request eventually succeeds on retry. Logs in `internal/cluster/` show bloom misses or stale routing decisions.

**Possible causes:**
- A peer node was just promoted or rejoined; its bloom filter has not yet been gossiped to the requesting node.
- Network partition or high RTT between cluster members is slowing the bloom gossip cycle.
- Clock skew between nodes is widening the bloom freshness window.
- Bloom filter false-negative window during rapid task churn (rare; the cluster falls back to scatter-gather).

**Resolution:**
1. Check cluster membership and per-peer gossip health in the `/v1/codeq/cluster/status` endpoint and metrics under `codeq_cluster_*`.
2. Verify pairwise connectivity and RTT between all cluster nodes; gossip is sensitive to multi-second partitions.
3. Ensure NTP is configured on every node — drift above a few seconds skews bloom freshness checks.
4. Confirm you are running the version with the cluster fix (`GetResult` cross-node routing + bloom on every ID-routed RPC + reaper context), commit `f315a14` or later.
5. If gossip lag is persistent, scatter-gather fallback will still return correct results — investigate the network layer rather than disabling cluster mode.

### 8. Webhook delivery failures

**Symptoms:** `codeq_webhook_deliveries_total{outcome="failure"}` rate is high.

**Possible causes:**
- Subscriber endpoint is down or returning non-2xx responses.
- DNS resolution failure for the subscriber URL.
- TLS certificate issues.
- Subscriber is rate-limiting codeQ callbacks.

**Resolution:**
1. Verify the subscriber endpoint is reachable from the codeQ API host.
2. Check TLS certificates and DNS configuration.
3. Review codeQ logs for HTTP response codes from the subscriber.
4. See [Webhooks](12-webhooks.md) for retry and backoff behavior.

### 9. Rate limit rejections (HTTP 429)

**Symptoms:** Clients receive `429 Too Many Requests`. `codeq_rate_limit_hits_total` increases.

**Possible causes:**
- Clients are sending requests faster than the configured rate.
- Rate limit configuration is too restrictive for the workload.
- Client retry logic is too aggressive (retry storm).

**Resolution:**
1. Check current rate limit configuration in `config.yml` under `rateLimit`.
2. Review the `Retry-After` header in 429 responses.
3. Ensure clients implement exponential backoff.
4. Increase `requestsPerMinute` and `burstSize` if limits are too low.
5. See [Operations § Rate limiting](10-operations.md#rate-limiting) for best practices.

### 10. Prometheus scrape failures

**Symptoms:** Gaps in Grafana dashboards. `up{job="codeq"} == 0`.

**Possible causes:**
- codeQ API is down.
- Network policy blocks Prometheus → codeQ traffic.
- Incorrect `targets` in `prometheus.yml`.

**Resolution:**
1. Verify codeQ is running and `/metrics` is reachable.
2. Confirm `prometheus.yml` has the correct `targets` list.
3. Check network policies / firewall rules between Prometheus and codeQ.

### 11. Tracing spans not appearing

**Symptoms:** Traces are missing in Jaeger or Tempo.

**Possible causes:**
- Tracing is not enabled in configuration (`tracingEnabled: false`).
- OTLP endpoint is misconfigured or unreachable.
- Sample ratio is set too low.

**Resolution:**
1. Set `tracingEnabled: true` and confirm `tracingOtlpEndpoint`.
2. Test connectivity to the OTLP endpoint: `grpcurl <endpoint> grpc.health.v1.Health/Check`.
3. Set `tracingSampleRatio: 1.0` temporarily to verify spans are exported.
4. See [Operations § Tracing](10-operations.md#tracing-opentelemetry) for configuration.

### 12. Build failure: `bytedance/sonic/loader` undefined symbols

**Symptoms:** Build fails with errors like `undefined: _func`, `undefined: moduledata` in `sonic/loader`.

**Cause:** The `bytedance/sonic` version is incompatible with the Go toolchain version.

**Resolution:**
1. Update sonic: `go get github.com/bytedance/sonic@latest`
2. Run `go mod tidy`
3. Verify the build: `go build ./...`

## Diagnostic PromQL queries

These queries help identify the source of common problems:

```promql
# Overall system health snapshot
max by (command, queue) (codeq_queue_depth)

# Task throughput (creation vs claim vs completion)
sum by (command) (rate(codeq_task_created_total[5m]))
sum by (command) (rate(codeq_task_claimed_total[5m]))
sum by (command, status) (rate(codeq_task_completed_total[5m]))

# Failure ratio
sum by (command) (rate(codeq_task_completed_total{status="FAILED"}[5m]))
/
sum by (command) (rate(codeq_task_completed_total[5m]))

# p95 latency by command (completed tasks)
histogram_quantile(0.95,
  sum by (le, command) (
    rate(codeq_task_processing_latency_seconds_bucket{status="COMPLETED"}[5m])
  )
)

# Webhook success rate
sum by (command, kind) (rate(codeq_webhook_deliveries_total{outcome="success"}[5m]))
/
sum by (command, kind) (rate(codeq_webhook_deliveries_total[5m]))
```

## Getting help

1. Check this guide and the [Operations](10-operations.md) documentation first.
2. Search [existing issues](https://github.com/osvaldoandrade/codeq/issues) for similar problems.
3. If the issue is new, open a GitHub issue with:
   - codeQ version and Go version
   - Configuration (redact secrets)
   - Relevant log output
   - Prometheus metrics or Grafana screenshots showing the issue
