# Operations

## Health

`GET /healthz` returns `{"status":"ok"}`.

## Metrics

`GET /metrics` exposes Prometheus-formatted metrics.

Notes:

- Queue/subscription gauges are collected from Redis during scrape. If you run multiple API replicas, use `max by (...)` in PromQL to avoid double-counting global values.
- `/metrics` is unauthenticated by default. Restrict access with ingress auth or network policy.

### Metric reference

| Metric | Type | Labels | Description |
|---|---|---|---|
| `codeq_queue_depth` | gauge | `command`, `queue` | Queue depth by command. `queue` is one of `ready`, `delayed`, `in_progress`, `dlq`. |
| `codeq_dlq_depth` | gauge | `command` | DLQ depth by command (same value as `codeq_queue_depth{queue="dlq"}`). |
| `codeq_task_created_total` | counter | `command` | Tasks created (enqueued). |
| `codeq_task_claimed_total` | counter | `command` | Tasks claimed by workers. |
| `codeq_task_completed_total` | counter | `command`, `status` | Tasks completed by final status (`COMPLETED`, `FAILED`). |
| `codeq_task_processing_latency_seconds` | histogram | `command`, `status` | End-to-end latency from task creation to completion. |
| `codeq_lease_expired_total` | counter | `command` | Lease expirations detected during claim-time repair. |
| `codeq_webhook_deliveries_total` | counter | `kind`, `command`, `outcome` | Webhook deliveries. `kind` is `queue_ready` (worker notification) or `task_result` (task completion callback). `outcome` is `success` or `failure`. |
| `codeq_rate_limit_hits_total` | counter | `scope`, `operation` | Rate limit rejections. `scope` is `producer`, `worker`, `webhook`, or `admin`. |
| `codeq_subscriptions_active` | gauge | `command` | Active subscriptions by event type (command). |

### Example PromQL

- Queue depth (global, multi-replica safe): `max by (command, queue) (codeq_queue_depth)`
- DLQ depth (global): `max by (command) (codeq_dlq_depth)`
- Task creation rate: `sum by (command) (rate(codeq_task_created_total[5m]))`
- Task completion rate by status: `sum by (command, status) (rate(codeq_task_completed_total[5m]))`
- p95 end-to-end latency (completed): `histogram_quantile(0.95, sum by (le, command) (rate(codeq_task_processing_latency_seconds_bucket{status="COMPLETED"}[5m])))`
- Rate limit hits: `sum by (scope, operation) (rate(codeq_rate_limit_hits_total[5m]))`

### Grafana dashboard

An example Grafana dashboard is provided at `docs/grafana/codeq-dashboard.json`.

## Tracing (OpenTelemetry)

codeQ supports distributed tracing via OpenTelemetry (OTLP gRPC exporter). When enabled, it emits spans for the task lifecycle and webhook deliveries, allowing you to trace a task from creation to completion.

Trace context propagation:

- Task records persist W3C trace context in `traceParent` / `traceState` (for cross-request correlation).
- Outgoing webhooks include W3C trace context headers (`traceparent` / `tracestate`), and may also include W3C `baggage` headers depending on the configured OpenTelemetry propagator.

### Configuration

Enable tracing via YAML:

````yaml
tracingEnabled: true
tracingServiceName: codeq
tracingOtlpEndpoint: "localhost:4317"
tracingOtlpInsecure: true
tracingSampleRatio: 1.0
````

Or via env vars:

- `TRACING_ENABLED=true`
- `TRACING_SERVICE_NAME=codeq` (or `OTEL_SERVICE_NAME`)
- `TRACING_OTLP_ENDPOINT=localhost:4317` (or `OTEL_EXPORTER_OTLP_ENDPOINT`)
- `TRACING_OTLP_INSECURE=true` (or `OTEL_EXPORTER_OTLP_INSECURE`)
- `TRACING_SAMPLE_RATIO=1.0`

### Jaeger / Tempo

Both Jaeger and Grafana Tempo support OTLP ingestion. Point `tracingOtlpEndpoint` (or `TRACING_OTLP_ENDPOINT`) at the collector/ingester endpoint (commonly port `4317` for OTLP gRPC).

## Background jobs

- Subscription cleanup: removes expired webhook subscriptions on a fixed interval.

Delayed queue moves and lease expiry requeue are performed during claim operations (claim-time repair), not by a background scanner.

## Cleanup

Admin cleanup removes all structures for tasks whose retention timestamp is <= cutoff. Use `limit` to bound latency.

## Rate limiting

Rate limiting is optional and disabled by default. When enabled, CodeQ uses a Redis-backed token bucket algorithm to enforce per-bearer-token rate limits.

### Configuration

Rate limits are configured per scope (producer, worker, webhook, admin). Each scope accepts two parameters:

- `requestsPerMinute`: Maximum sustained request rate per minute (0 = disabled)
- `burstSize`: Maximum burst capacity in tokens (0 = disabled)

Both values must be greater than zero to enable rate limiting for a scope.

Example configuration:

````yaml
rateLimit:
  producer:
    requestsPerMinute: 1000  # ~16.7 req/sec sustained
    burstSize: 100           # Allow bursts up to 100 requests
  worker:
    requestsPerMinute: 600   # ~10 req/sec sustained
    burstSize: 50
  webhook:
    requestsPerMinute: 600
    burstSize: 100
  admin:
    requestsPerMinute: 30    # ~0.5 req/sec sustained
    burstSize: 5
````

### Behavior

- **Token bucket algorithm**: Tokens refill continuously at `requestsPerMinute / 60` per second, up to `burstSize` capacity. Each request consumes 1 token.
- **Per-bearer-token granularity**: Rate limits are enforced per individual bearer token (SHA256-hashed for privacy in Redis keys).
- **Fail-open strategy**: If Redis is unreachable or returns an error during rate limit checks, the request is allowed to proceed. This prevents rate limiting infrastructure issues from causing API outages.
- **HTTP 429 responses**: When rate limit is exceeded, the API returns:
  - Status: `429 Too Many Requests`
  - Header: `Retry-After: <seconds>` indicating when to retry
  - Body: JSON with `error`, `scope`, `operation`, and `retryAfterSeconds` fields
- **Automatic TTL**: Token bucket state in Redis expires after ~2 refill cycles to prevent unbounded memory growth

### Applied endpoints

Rate limiting is currently applied to these endpoints when enabled:

- **Producer scope**: `POST /v1/codeq/tasks` (create task)
- **Worker scope**: `POST /v1/codeq/tasks/claim` (claim task)
- **Admin scope**: `POST /v1/codeq/admin/tasks/cleanup` (cleanup expired tasks)

**Note:** The `webhook` scope is configured but not yet implemented. It is reserved for future use to rate-limit internal webhook deliveries (queue_ready notifications and task result callbacks).

### Monitoring

Monitor rate limit rejections using the `codeq_rate_limit_hits_total` counter:

````promql
# Rate limit hits per scope
sum by (scope, operation) (rate(codeq_rate_limit_hits_total[5m]))

# Total rate limit rejections across all scopes
sum(rate(codeq_rate_limit_hits_total[5m]))
````

Alert when rate limit hits are sustained, which may indicate:
- Client misconfiguration or aggressive retry logic
- DDoS or abuse
- Rate limits set too low for legitimate traffic

### Best practices

**For production deployments:**

1. **Start conservative**: Begin with generous limits and tighten based on actual usage patterns
2. **Monitor before enforcing**: Deploy with high limits initially, observe metrics, then adjust
3. **Set burst > sustained**: Configure `burstSize` to handle legitimate traffic spikes (e.g., 2-5x sustained rate)
4. **Coordinate with clients**: Ensure clients implement proper retry logic with exponential backoff
5. **Different limits per scope**: Producers typically need higher limits than admin endpoints

**Example: high-throughput producer**

````yaml
rateLimit:
  producer:
    requestsPerMinute: 6000  # 100 req/sec sustained
    burstSize: 300           # Handle 3-second bursts at max capacity
````

**Example: rate-limited admin operations**

````yaml
rateLimit:
  admin:
    requestsPerMinute: 60    # 1 req/sec sustained
    burstSize: 10            # Small burst allowance
````

See [Configuration](14-configuration.md) for complete rate limiting configuration reference.

## Scaling

Scale horizontally. Use stateless API instances. KVRocks is the stateful component and must be scaled according to queue throughput.
