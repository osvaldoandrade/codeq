# Guide 2: K6 Load Testing & Interpretation

## Quick Start

**Run a scenario:**
```bash
docker compose --profile loadtest up -d codeq
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js
```

**Without Docker (if server running on localhost:8080):**
```bash
k6 run loadtest/k6/01_sustained_throughput.js
```

## The 6 Scenarios

Located in `loadtest/k6/`:

| Scenario | File | Purpose | Key Metrics |
|----------|------|---------|-------------|
| Sustained | `01_sustained_throughput.js` | Normal steady state | p99 claim latency, throughput |
| Burst | `02_burst_10k_10s.js` | Traffic spike handling | peak latency, error rate |
| Many Workers | `03_many_workers.js` | Worker scalability | contention, backoff behavior |
| Queue Depth | `04_prefill_queue.js` | Large backlogs | claim latency with depth |
| Priorities | `05_mixed_priorities.js` | Priority queue fairness | high-pri latency vs low-pri |
| Delayed | `06_delayed_tasks.js` | Scheduled tasks | delay accuracy, jitter |

## Customization

All scenarios accept environment variables:

```bash
# Higher throughput
RATE=100 k6 run loadtest/k6/01_sustained_throughput.js

# Longer test
DURATION=5m k6 run loadtest/k6/01_sustained_throughput.js

# More concurrent workers
WORKER_VUS=20 k6 run loadtest/k6/03_many_workers.js
```

## Interpreting Results

**Good indicators:**
- ✓ p99 claim latency < 100ms under normal load
- ✓ Error rate < 0.1%
- ✓ No timeouts during sustained throughput

**Red flags:**
- ✗ p99 grows significantly with queue depth (poor scaling)
- ✗ High error rate with burst traffic (backpressure not working)
- ✗ Priority inversion (low-priority tasks claimed before high-priority)

## Correlating with Prometheus

While k6 runs, query metrics:

```bash
# Access Prometheus at http://localhost:9090
# Key queries:
# - rate(codeq_task_created_total[1m]) - task creation rate
# - codeq_queue_depth - current backlog by command
# - rate(codeq_task_claimed_total[1m]) - processing rate
# - codeq_lease_repair_duration_seconds - health check overhead
```

## Debug Slow Claims

If p99 is high, check:

1. **Server logs:** Look for GC pauses, lease repair taking long
2. **Goroutine count:** `curl http://localhost:6060/debug/pprof/goroutine | grep -c goroutine`
3. **Memory stats:** `curl http://localhost:6060/debug/pprof/heap?debug=1 | head -20`

## Before/After Testing

```bash
# Establish baseline
k6 run --summary-export=baseline.json loadtest/k6/01_sustained_throughput.js

# Make optimization
# ...code changes...

# Test again
k6 run --summary-export=optimized.json loadtest/k6/01_sustained_throughput.js

# Compare JSON outputs for latency/throughput deltas
```
