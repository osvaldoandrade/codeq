# k6 Load Testing Guide for CodeQ

## Overview

k6 provides realistic HTTP load testing across six production scenarios: sustained throughput, burst handling, many concurrent workers, large queue depth, priority mixing, and delayed tasks. Each scenario accepts environment variables for tuning—scale them down for quick local tests, up for deeper analysis.

## Key Files

- `loadtest/k6/`: Six scenario scripts (01-06)
- `loadtest/k6/lib/config.js`: Configuration library
- `loadtest/README.md`: Scenario descriptions

## Quick Start (Docker Compose)

### Start the stack
```bash
docker compose up -d
```

Optional (adds Prometheus + Grafana metrics):
```bash
docker compose --profile obs up -d
```

### Run a scenario
```bash
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

## Scenarios and Use Cases

### 1. Sustained Throughput (steady load)
**Good for**: Detecting latency degradation under moderate sustained load
```bash
RATE=1000 DURATION=1h WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```
For quick local tests, use: `RATE=100 DURATION=1m WORKER_VUS=10`

### 2. Burst Load (sudden spike)
**Good for**: Validating queue recovery and backpressure handling
```bash
RATE=1000 BURST_DURATION=10s DRAIN_DURATION=5m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/02_burst_10k_10s.js
```
For quick tests: `RATE=100 BURST_DURATION=10s DRAIN_DURATION=1m WORKER_VUS=10`

### 3. Many Workers (100+ concurrent claimers)
**Good for**: Validating high-concurrency claim operations
```bash
WORKER_VUS=150 DURATION=10m PRODUCER_RATE=800 \
  docker compose --profile loadtest run --rm k6 run /scripts/03_many_workers.js
```
For quick tests: `WORKER_VUS=20 DURATION=2m PRODUCER_RATE=100`

### 4. Large Queue Depth (100K+ pending)
**Good for**: Testing latency impact of queue accumulation
```bash
TASKS=100000 VUS=200 \
  docker compose --profile loadtest run --rm k6 run /scripts/04_prefill_queue.js
```
For quick tests: `TASKS=10000 VUS=20`

### 5. Mixed Priorities (50% high, 30% medium, 20% low)
**Good for**: Validating priority queue correctness
```bash
RATE=1000 DURATION=10m WORKER_VUS=300 \
  docker compose --profile loadtest run --rm k6 run /scripts/05_mixed_priorities.js
```

### 6. Delayed Tasks (50% with delaySeconds)
**Good for**: Testing delayed task scheduling
```bash
RATE=500 DURATION=10m WORKER_VUS=200 DELAY_PCT=50 MIN_DELAY_SECONDS=1 MAX_DELAY_SECONDS=30 \
  docker compose --profile loadtest run --rm k6 run /scripts/06_delayed_tasks.js
```

## Environment Variables

| Variable | Script default | Docker Compose default | Purpose |
|----------|----------------|------------------------|---------|
| `CODEQ_BASE_URL` | `http://localhost:8080` | `http://codeq:8080` | API endpoint |
| `CODEQ_PRODUCER_TOKEN` | `dev-token` | `dev-token` | Producer auth |
| `CODEQ_WORKER_TOKEN` | `dev-token` | `dev-token` | Worker auth |
| `RATE` | 500 | 1000 | Enqueue rate (tasks/sec) |
| `DURATION` | 5m | 1m | Test duration |
| `WORKER_VUS` | 100 | 300 | Virtual users (worker threads) |
| `CLAIM_P99_MS` | 100 | N/A | Success threshold for claim p99 latency |

## Measurement Strategy

1. **Before/After baseline**:
   - Run scenario twice on main branch (validate consistency)
   - Run on your branch
   - Compare p99 latency, success rate, error rate

2. **Expected metrics in output**:
   - `http_reqs`: Total HTTP requests
   - `http_req_duration{p99}`: p99 latency in ms
   - `checks`: Pass/fail assertions (e.g., p99 < 100ms)

3. **Success criteria**:
   - No increase in p99 latency
   - Zero check failures
   - No timeout errors

## Common Pitfalls

- **Too many VUS locally**: High VUS on 4-core machine = CPU saturation, not queue saturation. Start with 10-20 VUS for local testing.
- **Network overhead**: Docker-compose uses internal bridge; k6 runs in-container (fast). Real network tests may show higher latency.
- **Token mismatches**: Verify `CODEQ_PRODUCER_TOKEN` and `CODEQ_WORKER_TOKEN` match `docker-compose.yml`
- **Port conflicts**: If ports already in use, docker-compose will fail; stop existing containers first

## Monitoring During Tests

### Using Prometheus + Grafana (if started with `--profile obs`)
1. Grafana: http://localhost:3000 (default creds: admin/admin)
2. Query: `rate(codeq_task_enqueue_total[1m])` for enqueue rate
3. Query: `codeq_queue_depth` for queue depth over time

### Using k6 real-time stats
- k6 prints live progress; look for:
  - `✓ check_claim_p99` (assertions pass)
  - `http_req_duration`: Latency histogram
  - Check for `errors` or `failed` counts

## Next Steps for Optimization

If scenario shows latency increase:
1. Check CPU usage: `docker stats`
2. Look at queue depth: `/v1/codeq/admin/queues/:command`
3. Profile with `--profile obs` and check Prometheus for queue depth, CPU
4. Move to Go benchmarks if issue is in hot path
