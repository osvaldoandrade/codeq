# Load Testing

This document describes the load testing harness shipped with the repository (Issue #30).

## What’s Included

- **k6 HTTP scenarios** in `loadtest/k6/` for producer + worker traffic.
- **Go benchmarks** in `internal/bench/` for quick perf regression checks.
- **Observability stack** via `docker compose --profile obs up -d` (Prometheus + Grafana) to view `/metrics`.

## Running k6 Scenarios (Local)

Start a local stack:

```bash
docker compose up -d
docker compose --profile obs up -d   # optional: Prometheus + Grafana
```

Run a scenario:

```bash
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

### Environment Variables

The k6 container is pre-configured by `docker-compose.yml`, but you can override:

- `CODEQ_BASE_URL` (default inside compose network: `http://codeq:8080`)
- `CODEQ_PRODUCER_TOKEN` (default: `dev-token`)
- `CODEQ_WORKER_TOKEN` (default: `dev-token`)
- `CODEQ_COMMANDS` (default: `GENERATE_MASTER`)

Scenario scripts also accept:

- `RATE`, `DURATION`
- `WORKER_VUS`
- `CLAIM_P99_MS` (used by `01_sustained_throughput.js`)
- `TASKS`, `VUS` (used by `04_prefill_queue.js`)

## Scenarios

The following scripts map directly to the scenarios listed in Issue #30:

- Sustained throughput: `loadtest/k6/01_sustained_throughput.js`
- Burst load: `loadtest/k6/02_burst_10k_10s.js`
- Many workers: `loadtest/k6/03_many_workers.js`
- Large queue depth: `loadtest/k6/04_prefill_queue.js`
- Mixed priorities: `loadtest/k6/05_mixed_priorities.js`
- Delayed tasks: `loadtest/k6/06_delayed_tasks.js`

## Measuring and Interpreting Results

- Use k6 output for request latency percentiles and error rates.
- Use `/metrics` (Prometheus) to correlate:
  - `codeq_task_created_total`
  - `codeq_task_claimed_total`
  - `codeq_task_completed_total`
  - `codeq_queue_depth{command=...,queue=...}`
- Use `/v1/codeq/admin/queues/:command` to validate queue depth and backlog behavior during tests.

## Go Benchmarks

Run benchmarks:

```bash
go test ./internal/bench -bench . -benchtime=30s
```

These benchmarks run in-process (Gin engine + miniredis) and are intended to catch regressions and compare branches.

