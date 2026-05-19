# Observability: Metrics

Metrics are the quantitative pillar. Where tracing answers "which step took the time for this one task" and logging answers "what did this one task carry," metrics answer "how many, how fast, at what percentile, with what error rate, since when." They are the right tool whenever the operative word in the question is a number rather than a noun. CodeQ emits Prometheus metrics through the same in-process registry that any Go service using `prometheus/client_golang` would, exposes them at `GET /metrics` on the API port, and treats the metric definitions as a stable contract that operators write dashboards and alerts against.

This page documents the exact metrics the broker exports, the labels they carry, where they are observed in code, the rationale for the choice of counter versus histogram in each case, the bucket boundaries used for the latency histogram, and how to point a Prometheus scrape job at the broker. It assumes a working familiarity with the Prometheus exposition format and PromQL.

## The exposition endpoint

The metrics endpoint is registered in `pkg/app/url_mappings.go:12`:

```go
app.Engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
```

`promhttp.Handler()` returns an HTTP handler bound to the default Prometheus registry. CodeQ uses the default registry through `prometheus.MustRegister` calls in `internal/metrics/metrics.go:73-83`, so every metric defined at package init is automatically visible at the endpoint. The endpoint serves on the API port — by default `:8080` — under the same Gin engine that serves `/v1/codeq/*`. There is no authentication on `/metrics`. The standard deployment posture is to bind `:8080` only to a private network or to a service mesh that enforces ingress controls, and to let your Prometheus scraper into that same network. If you need to expose the broker's API to the public internet without exposing the metrics endpoint, run the scraper inside a sidecar that pulls from `localhost:8080/metrics` and republishes through your own auth.

A minimal Prometheus scrape config looks like this:

```yaml
scrape_configs:
  - job_name: codeq
    metrics_path: /metrics
    scrape_interval: 15s
    static_configs:
      - targets: ['codeq-broker-0:8080', 'codeq-broker-1:8080', 'codeq-broker-2:8080']
```

A 15-second scrape interval is the canonical default. The cost of scraping more frequently is a denser TSDB and faster reaction to alerts; the cost of scraping less frequently is that short spikes get smeared. For a broker that processes tens of thousands of tasks per second, 15 seconds is enough resolution to see most operational events without pathological storage growth. The exposition is plain text in Prometheus format, with `# HELP` and `# TYPE` lines preceding each metric family, which is the same format the OpenMetrics spec consumes if you prefer to read it through an OpenMetrics-aware client.

## The metric catalog

All counters and the latency histogram are defined in `internal/metrics/metrics.go`. The namespace is `codeq`, so the fully qualified names are `codeq_task_created_total`, `codeq_task_claimed_total`, and so on. The redis-backed depth gauges are defined in `internal/metrics/redis_collector.go` with names `codeq_queue_depth`, `codeq_dlq_depth`, and `codeq_subscriptions_active` — those use the `prometheus.Collector` interface to pull values from the persistence layer on each scrape rather than incrementing in-process counters, which is the right design for depth-style metrics that have a single source of truth outside the process.

### Lifecycle counters

`codeq_task_created_total{command}` is incremented every time a task is enqueued. The label is the command name (`generate_master`, `generate_creative`, etc.). The increment happens at `internal/repository/task_repository.go:439` and `internal/repository/pebble/task_repository.go:290`, immediately after the durable write of the task record. Counting at the repository layer rather than the controller layer means the counter reflects what actually made it to durable storage rather than what was attempted; a request that failed validation at the controller does not bump the counter.

`codeq_task_claimed_total{command}` is incremented every time a worker successfully claims a task. The increment happens at multiple call sites in `internal/services/scheduler_service.go:166, 180, 199, 213` to cover both the single-claim and batch-claim paths. Like create, the counter reflects successful claims only; a 429 rate-limit rejection does not bump it (that goes to `codeq_rate_limit_hits_total` instead).

`codeq_task_completed_total{command, status}` is incremented every time a task reaches a terminal state. The `status` label is the final status of the task — typically `succeeded` or `failed`. The increment happens in `internal/services/results_service.go:177` for explicit result submissions and in `internal/repository/task_repository.go:1088`, `internal/repository/pebble/task_repository.go:983`, and `internal/repository/pebble/reaper.go:245` for lifecycle-driven completions (such as a task that exceeded its retry budget and was DLQ'd). Splitting by status is what lets you compute a failure rate as a PromQL expression: `rate(codeq_task_completed_total{status="failed"}[5m]) / rate(codeq_task_completed_total[5m])`.

`codeq_lease_expired_total{command}` is incremented every time the broker detects that a previously claimed task's lease has expired without a heartbeat or completion. The increment happens at `internal/repository/task_repository.go:567`, `internal/repository/pebble/task_repository.go:1200`, and `internal/repository/pebble/reaper.go:195`. A nonzero rate on this metric means workers are dying mid-task, getting partitioned from the broker, or running over their configured lease. Sustained growth here is one of the strongest leading indicators of an infrastructure problem.

### Webhook and rate limiting counters

`codeq_webhook_deliveries_total{kind, command, outcome}` is incremented every time the broker attempts a webhook delivery. The `kind` label distinguishes `task_result` (terminal result callback) from `queue_ready` (queue-ready notification). The `outcome` label is `success` or `failure`. The increments happen in `internal/services/result_callback_service.go:129, 147` and `internal/services/notifier_service.go:173, 179, 183`. A rising failure rate here, broken down by command, is how you discover that a particular tenant's webhook receiver is down or rate-limiting you.

`codeq_rate_limit_hits_total{scope, operation}` is incremented every time the broker rejects a request at the rate-limit middleware. The `scope` label is one of `producer`, `worker`, `admin`, or `webhook`. The `operation` label is the API surface being rate-limited (`tasks`, `claim`, `task_result`, etc.). The increment happens at `internal/middleware/rate_limit.go:59` for HTTP-side limits and at `internal/services/result_callback_service.go:114` and `internal/services/notifier_service.go:150` for webhook-side limits. A nonzero rate is normal under load; a spike correlated with a drop in `codeq_task_created_total` tells you the rate limiter is shedding traffic.

### Latency histogram

`codeq_task_processing_latency_seconds{command, status}` is the one histogram in the broker's metric set. It records the end-to-end latency from task creation to terminal completion, observed at `internal/services/results_service.go:179` and the equivalent paths in the repository layer. The buckets, declared at `internal/metrics/metrics.go:40`, are:

```
0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600
```

The bucket spacing is roughly logarithmic, with a denser cluster between 100ms and 5 seconds where most useful SLOs sit and a sparse tail out to one hour for tasks that legitimately take that long. The Prometheus convention for histograms is that each bucket has a `_bucket{le="..."}` series, plus a `_sum` and a `_count`, which together let `histogram_quantile` compute any percentile by linear interpolation within a bucket.

### Depth gauges

`codeq_queue_depth{command, queue}` reports the current depth of the ready, in-progress, and delayed queues for each command. `codeq_dlq_depth{command}` reports the current size of the dead-letter queue. `codeq_subscriptions_active{command}` reports the current number of active worker subscriptions per command. These three series are defined in `internal/metrics/redis_collector.go:33-50` and emitted by a custom `prometheus.Collector` whose `Collect` method queries the persistence layer on each scrape with a two-second bounded timeout. Depth metrics are the right shape for gauges rather than counters because the question is "how much is there right now," not "how much has accumulated over time."

## Why histograms beat counters for latency

The single most common mistake in metrics design is to track latency with a counter and a sum and divide. The resulting "average latency" answers a question no operator actually has. Average latency hides the tail. If 99 requests complete in 10ms and one request completes in 10 seconds, the average is 110ms, which sounds healthy; the p99 is 10 seconds, which is what your users actually experienced on the call that mattered. CodeQ uses a histogram, not a sum-and-count, because the SLO question is always a percentile question.

The way a Prometheus histogram works is that each bucket is itself a counter. The `le="0.5"` series counts how many observations were less than or equal to half a second; the `le="1"` series counts how many were less than or equal to one second; and so on. The buckets are cumulative. To compute the p99 you ask: what bucket boundary contains the 99th-percentile observation? Prometheus's `histogram_quantile` function does the bookkeeping: it takes a rate over the bucket counters, finds the bucket whose count crosses the 99% threshold, and linearly interpolates within that bucket to produce a single number. The result is approximate — bounded by the bucket width — but bounded approximation is the right trade against the alternative of exporting every single observation.

An alert that uses the histogram looks like this:

```promql
histogram_quantile(0.99,
  sum by (le, command) (
    rate(codeq_task_processing_latency_seconds_bucket[5m])
  )
) > 2
```

That expression says "over the last five minutes, for each command, the 99th-percentile end-to-end latency exceeded two seconds." It is a single PromQL line that fires the page that opened the worked example in [Observability Overview](Observability-Overview). The `sum by (le, command)` is what makes the percentile per-command rather than global — you almost always want the per-dimension percentile because a single bad command can hide behind a fleet of healthy ones in the global rollup.

A histogram cannot answer "what was the exact latency of task X" — that is what tracing and logs are for. What it answers is "what fraction of all tasks exceeded this latency threshold," which is the question every meaningful SLO is built on.

## Per-tenant labels and cardinality

CodeQ deliberately does not label its metrics with `tenant_id`. The reason is cardinality. Prometheus stores one time series per unique label combination; ten tenants times ten commands times two statuses is two hundred series, which is small. Ten thousand tenants times ten commands times two statuses is two hundred thousand series, which is enough to make a small Prometheus instance struggle. Per-tenant metrics live in tracing (where `codeq.tenant_id` is a span attribute) and in logs (where it is a structured field), both of which scale through different mechanisms.

If your deployment has a small enough tenant count that per-tenant metrics are tractable, you can add the label by wrapping the counters with a `WithLabelValues(tenant, command)` call at the observation sites. The change is mechanical but the operational consequence is permanent — once a label is added it is very hard to remove without breaking dashboards — so think about your tenant count before reaching for it. The recommendation for high-tenant-count deployments is to rely on traces for per-tenant attribution and reserve metrics for fleet-level rollups.

## Reading a scrape: a worked walkthrough

Suppose you `curl http://codeq-broker:8080/metrics` and get a wall of text. The interesting part for end-to-end latency looks roughly like this:

```
# HELP codeq_task_processing_latency_seconds End-to-end latency from task creation to completion (seconds).
# TYPE codeq_task_processing_latency_seconds histogram
codeq_task_processing_latency_seconds_bucket{command="generate_master",status="succeeded",le="0.1"} 12847
codeq_task_processing_latency_seconds_bucket{command="generate_master",status="succeeded",le="0.25"} 14920
codeq_task_processing_latency_seconds_bucket{command="generate_master",status="succeeded",le="0.5"} 15110
codeq_task_processing_latency_seconds_bucket{command="generate_master",status="succeeded",le="1"} 15187
... etc up to le="+Inf"
codeq_task_processing_latency_seconds_sum{command="generate_master",status="succeeded"} 1842.7
codeq_task_processing_latency_seconds_count{command="generate_master",status="succeeded"} 15201
```

Read top to bottom, each `_bucket` line says "this many observations were less than or equal to this latency." The `_count` is the total observations. The `_sum` is the total of all observed values. From this snapshot you can read off that about 85% of generate_master tasks completed within 100ms (12847/15201), 98% within 250ms, and the long tail beyond one second is fewer than 14 tasks. The Prometheus server stores each `_bucket`, `_sum`, and `_count` as its own time series, so you can graph any percentile at any time by running `histogram_quantile` on a `rate` of the bucket series.

The lifecycle counters look like this:

```
# HELP codeq_task_created_total Total number of tasks created (enqueued).
# TYPE codeq_task_created_total counter
codeq_task_created_total{command="generate_master"} 15201
codeq_task_created_total{command="generate_creative"} 8814
```

A counter is just a monotonically increasing integer. You almost never look at the raw value; what you graph is the `rate()` over a window, which gives you tasks per second.

## Recording rules and alert design

A common pattern is to precompute heavy aggregations as Prometheus recording rules. For CodeQ, useful recording rules include:

- `codeq:tasks_per_second:rate5m` = `sum by (command) (rate(codeq_task_created_total[5m]))`
- `codeq:failure_ratio:rate5m` = `sum by (command) (rate(codeq_task_completed_total{status="failed"}[5m])) / sum by (command) (rate(codeq_task_completed_total[5m]))`
- `codeq:p99_latency:rate5m` = `histogram_quantile(0.99, sum by (le, command) (rate(codeq_task_processing_latency_seconds_bucket[5m])))`

Precomputing these has two benefits. First, the dashboards stay fast even when the underlying series are large, because Grafana queries the cheap recorded series rather than recomputing histograms on every render. Second, alerts share the same time-series as dashboards, so the page you receive matches the graph you open — no surprises from window-mismatched aggregations. Three reasonable alerts for a production deployment are: failure ratio above 1%, p99 latency above SLO, and queue depth growing faster than completion rate (which signals a backlog forming).

## What metrics do not tell you

A metric tells you the shape of a population, not the identity of any individual. If `codeq_task_completed_total{status="failed"}` rose by 100 in the last minute, you know 100 tasks failed but you do not know *which* tasks, with what payload, and under what trace ID. For that you must pivot — to [Observability Logging](Observability-Logging) for the structured records of those specific tasks, and from there to [Observability Tracing](Observability-Tracing) using the `traceparent` field on the log lines. Metrics also cannot tell you why a function is slow internally; that is the territory of [Observability Profiling](Observability-Profiling), and the canonical workflow is to use the metric to confirm there is something to investigate, then use a profile to find out what. The metrics endpoint is the entry point to the observability stack; it is rarely the destination.
