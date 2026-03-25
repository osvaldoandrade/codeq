# Troubleshooting

This guide covers common issues, their symptoms, and resolution steps. For metric definitions and PromQL examples see [Operations](10-operations.md). For alerting rule configuration see `deploy/docker-compose/local-dev/alerting-rules.yml`.

## Quick diagnostics

| Check | Command / URL | Healthy response |
|---|---|---|
| API health | `GET /healthz` | `{"status":"ok"}` |
| Prometheus scrape | `GET /metrics` | Prometheus text format, no errors |
| KVRocks connectivity | `redis-cli -h <host> -p 6379 ping` | `PONG` |

## Common issues

### 1. API returns `503` or does not start

**Symptoms:** `/healthz` is unreachable or returns an error.

**Possible causes:**
- KVRocks is unreachable or slow to accept connections.
- Port conflict on the configured listen port.
- Invalid YAML configuration file.

**Resolution:**
1. Verify KVRocks is running: `redis-cli -h <host> -p 6379 ping`.
2. Check the codeQ log output for startup errors.
3. Validate `config.yml` syntax with a YAML linter.
4. Ensure the listen port is not already in use.

### 2. Tasks stuck in ready queue (not being claimed)

**Symptoms:** `codeq_queue_depth{queue="ready"}` grows while `codeq_task_claimed_total` rate is zero.

**Possible causes:**
- No workers are running or connected.
- Workers are polling for a different `command` value.
- Network partition between workers and the API.

**Resolution:**
1. Confirm at least one worker is polling `POST /v1/codeq/tasks/claim` with the correct `command`.
2. Check worker logs for connection errors.
3. Verify network connectivity between worker and API.

### 3. Growing dead-letter queue (DLQ)

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

### 4. High end-to-end latency

**Symptoms:** `histogram_quantile(0.95, ...)` on `codeq_task_processing_latency_seconds` is above expected SLO.

**Possible causes:**
- KVRocks is under memory or CPU pressure.
- Too few workers for the queue throughput.
- Large task payloads increasing serialization time.
- Network latency between API and KVRocks.

**Resolution:**
1. Check KVRocks resource utilization (`INFO` command).
2. Scale workers horizontally.
3. Review [Performance Tuning](17-performance-tuning.md) for KVRocks configuration.
4. Consider reducing `artifactIn` payload size.

### 5. Lease expirations spiking

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

### 6. Webhook delivery failures

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

### 7. Rate limit rejections (HTTP 429)

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

### 8. Prometheus scrape failures

**Symptoms:** Gaps in Grafana dashboards. `up{job="codeq"} == 0`.

**Possible causes:**
- codeQ API is down.
- Network policy blocks Prometheus → codeQ traffic.
- Incorrect `targets` in `prometheus.yml`.

**Resolution:**
1. Verify codeQ is running and `/metrics` is reachable.
2. Confirm `prometheus.yml` has the correct `targets` list.
3. Check network policies / firewall rules between Prometheus and codeQ.

### 9. Tracing spans not appearing

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

### 10. Build failure: `bytedance/sonic/loader` undefined symbols

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
