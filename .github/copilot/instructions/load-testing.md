# Load Testing Guide

## Running k6 Scenarios

### Using Docker Compose

Start infrastructure:

```bash
# Basic setup (codeQ + KVRocks)
docker compose up -d

# With observability (Prometheus + Grafana)
docker compose --profile obs up -d

# Start load testing k6 service
docker compose --profile loadtest up
```

Run specific scenario:

```bash
# Sustained throughput (1000 tasks/sec)
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Burst load (10k tasks in 10s)
RATE=10000 DURATION=10s docker compose --profile loadtest run --rm k6 run /scripts/02_burst_load.js
```

### Local k6 Execution (without Docker)

Install k6: `brew install k6` (macOS) or download from k6.io

```bash
# Set environment for local server
export CODEQ_BASE_URL=http://localhost:8080

# Run scenario
k6 run loadtest/k6/01_sustained_throughput.js
```

## Key Metrics to Monitor

### From k6 Output

- **http_reqs**: Total requests made
- **http_req_duration**: Latency distribution (p50, p90, p99)
- **http_req_failed**: Failed requests (auth, errors, timeouts)
- **vus**: Virtual users active at any moment

Example output:
```
     http_req_duration....................: avg=28.41ms  min=8.21ms  med=24.31ms max=156.21ms p(90)=45.21ms p(99)=89.43ms
     http_reqs............................. 120000 in 60.1s
```

### Interpretation

- **p99 latency >100ms**: Potential bottleneck under load
- **Failed requests >0.1%**: Investigate timeout or resource exhaustion
- **VU scaling**: Linear throughput scaling indicates healthy concurrency

## Prometheus Metrics (with obs profile)

Access Grafana at `http://localhost:3000`:

1. **Queue depth**: Should stabilize around target (e.g., 100 tasks for 1000 req/s with 5s processing)
2. **Task completion rate**: Should match creation rate at steady state
3. **Redis commands/sec**: Shows persistence layer efficiency
4. **Worker claim rate**: Indicates claim operation performance

## Identifying Bottlenecks

| Symptom | Likely Cause | Debug Step |
|---------|--------------|-----------|
| p99 latency spikes > every 10s | GC pauses | Check memory allocations |
| Linear throughput decline | Connection pooling exhaustion | Monitor Redis connections |
| Sudden p99 spike with low CPU | Redis lock contention | Profile Redis command latencies |
| Failed requests during burst | Memory exhaustion | Check task queue depth limits |

## Realistic Load Profiles

### Small deployment (1 worker)
```bash
RATE=100 DURATION=10m WORKER_VUS=10 k6 run loadtest/k6/01_sustained_throughput.js
```

### Medium deployment (10 workers)
```bash
RATE=1000 DURATION=30m WORKER_VUS=100 k6 run loadtest/k6/01_sustained_throughput.js
```

### High scale (50+ workers)
```bash
RATE=10000 DURATION=1h WORKER_VUS=500 k6 run loadtest/k6/01_sustained_throughput.js
```

## Measurement Best Practices

1. **Warm-up period**: Run first 2 minutes as warm-up (discarded from metrics)
2. **Steady state**: Run main test for ≥10 minutes to capture variability
3. **Cool-down**: Stop gradually (ramp down VUs) to see graceful shutdown
4. **Baseline**: Establish baseline on unmodified code before optimization
5. **Isolate variables**: Change one thing per test run
