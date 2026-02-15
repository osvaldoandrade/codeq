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

## Background jobs

- Subscription cleanup: removes expired webhook subscriptions on a fixed interval.

Delayed queue moves and lease expiry requeue are performed during claim operations (claim-time repair), not by a background scanner.

## Cleanup

Admin cleanup removes all structures for tasks whose retention timestamp is <= cutoff. Use `limit` to bound latency.

## Rate limiting

Rate limiting is optional and disabled by default. When enabled, API endpoints return `429 Too Many Requests` with a `Retry-After` header.

Example configuration:

```yaml
rateLimit:
  producer:
    requestsPerMinute: 1000
    burstSize: 100
  worker:
    requestsPerMinute: 600
    burstSize: 50
  webhook:
    requestsPerMinute: 600
    burstSize: 100
  admin:
    requestsPerMinute: 30
    burstSize: 5
```

## Scaling

Scale horizontally. Use stateless API instances. KVRocks is the stateful component and must be scaled according to queue throughput.
