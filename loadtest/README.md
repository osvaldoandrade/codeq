# Load Testing

This directory contains a practical load/performance testing harness for codeQ:

- `loadtest/k6/`: HTTP load scenarios (producers + workers)
- `internal/bench/`: Go benchmarks for fast local perf regression checks

## Quick Start (Docker Compose + k6)

1. Start codeQ + KVRocks:

```bash
docker compose up -d
```

Optional (recommended): start the observability stack (Prometheus + Grafana):

```bash
docker compose --profile obs up -d
```

2. Run a scenario with the bundled k6 container:

```bash
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

## Scenarios (Issue #30)

All scripts accept environment variables so you can scale up/down depending on your machine.

### 1) Sustained throughput (1000 tasks/sec for 1 hour)

```bash
RATE=1000 DURATION=1h WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

### 2) Burst load (10,000 tasks in 10 seconds)

```bash
RATE=1000 BURST_DURATION=10s DRAIN_DURATION=5m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/02_burst_10k_10s.js
```

### 3) Many workers (100+ concurrent claimers)

```bash
WORKER_VUS=150 DURATION=10m PRODUCER_RATE=800 \
  docker compose --profile loadtest run --rm k6 run /scripts/03_many_workers.js
```

### 4) Large queue depth (100K+ pending tasks)

This fills the queue (no workers). Use admin stats and `/metrics` to observe impact.

```bash
TASKS=100000 VUS=200 \
  docker compose --profile loadtest run --rm k6 run /scripts/04_prefill_queue.js
```

### 5) Mixed priorities (50% high, 30% medium, 20% low)

```bash
RATE=1000 DURATION=10m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/05_mixed_priorities.js
```

### 6) Delayed tasks (50% with delaySeconds)

```bash
RATE=500 DURATION=10m WORKER_VUS=200 DELAY_PCT=50 MIN_DELAY_SECONDS=1 MAX_DELAY_SECONDS=30 \
  docker compose --profile loadtest run --rm k6 run /scripts/06_delayed_tasks.js
```

## Common Environment Variables

- `CODEQ_BASE_URL`:
  - k6 script default (in `loadtest/k6/lib/config.js`): `http://localhost:8080`
  - Docker Compose default (in `docker-compose.yml`): `http://codeq:8080` inside the
    Compose network
  - to override when running via Docker Compose:
    `CODEQ_BASE_URL=http://your-host:8080 docker compose --profile loadtest run --rm k6 …`
- `CODEQ_PRODUCER_TOKEN`: defaults to `dev-token` (matches `docker-compose.yml`)
- `CODEQ_WORKER_TOKEN`: defaults to `dev-token` (matches `docker-compose.yml`)
- `CODEQ_COMMANDS`: comma-separated commands to target (default `GENERATE_MASTER`)

## Success Criteria Hooks

- P99 claim latency threshold is controlled by `CLAIM_P99_MS` (default `100`) in
  `loadtest/k6/01_sustained_throughput.js`.
- Queue depth can be tracked via `/v1/codeq/admin/queues/:command` and `/metrics`.

## Go Benchmarks (Fast Regression Checks)

These benchmarks run fully in-process (Gin engine + miniredis) and are intended for
quick comparisons between branches/commits.

```bash
go test ./internal/bench -bench . -benchtime=30s
```

