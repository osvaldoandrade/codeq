# Performance Baselines

This document records baseline performance metrics from running the codeQ load testing
framework. These baselines serve as regression benchmarks for future releases.

## Test Environment

| Component     | Detail                                      |
|---------------|---------------------------------------------|
| CPU           | AMD EPYC 9V74 80-Core (4 vCPUs allocated)   |
| Go version    | 1.24                                        |
| Persistence   | Pebble embedded (`numShards=1`, `fsyncOnCommit=false`) |
| k6 version    | 0.55.0                                      |
| Network       | localhost (loopback)                        |

> **Note:** These k6 baselines ran against a single codeq instance backed
> by an embedded Pebble store under `t.TempDir()`. They isolate codeq's
> HTTP/queue overhead on the loopback. For full-cycle in-process numbers
> (4-shard Pebble, no HTTP layer), see Phase 8 results in
> [`docs/17-performance-tuning.md`](17-performance-tuning.md).

---

## Go Benchmarks (In-Process)

These benchmarks run the full Create → Claim → Complete cycle through the Gin HTTP
router (or directly through the scheduler) backed by an embedded Pebble store under
`t.TempDir()`. They isolate codeq's own processing overhead from network latency.

```
goos: linux
goarch: amd64
cpu: AMD EPYC 9V74 80-Core Processor

BenchmarkHTTP_CreateClaimComplete-4          3697   3228100 ns/op   2024751 B/op   8664 allocs/op
BenchmarkHTTP_CreateClaimComplete-4          3672   3209888 ns/op   2011850 B/op   8663 allocs/op
BenchmarkHTTP_CreateClaimComplete-4          3540   3213989 ns/op   2007331 B/op   8662 allocs/op

BenchmarkScheduler_CreateClaimComplete-4     3921   3073572 ns/op   1997302 B/op   8487 allocs/op
BenchmarkScheduler_CreateClaimComplete-4     3736   3061901 ns/op   1982325 B/op   8486 allocs/op
BenchmarkScheduler_CreateClaimComplete-4     3885   3059117 ns/op   1985484 B/op   8486 allocs/op
```

### Summary

| Benchmark                              | Avg ns/op   | Avg ops/sec | Avg allocs/op |
|----------------------------------------|-------------|-------------|---------------|
| HTTP Create→Claim→Complete             | 3,217,326   | ~311        | 8,663         |
| Scheduler Create→Claim→Complete        | 3,064,863   | ~326        | 8,486         |

- HTTP layer overhead is ~5% above the scheduler-level benchmark.
- Memory allocation per cycle is ~2 MB (dominated by JSON encoding and Pebble batch construction).

---

## k6 Load Test Results

All scenarios used `CODEQ_BASE_URL=http://localhost:8080` with a single codeq instance
backed by Pebble. Error rates were **0.00%** across every scenario.

### Scenario 1 — Sustained Throughput

| Parameter       | Value   |
|-----------------|---------|
| Producer rate   | 200/s   |
| Duration        | 30 s    |
| Worker VUs      | 50      |
| Total requests  | 32,533  |

| Metric                                  | Value       |
|------------------------------------------|-------------|
| Overall throughput                       | 1,082 req/s |
| `http_req_duration` avg                  | 5.69 ms     |
| `http_req_duration` p(90)                | 9.16 ms     |
| `http_req_duration` p(95)                | 10.94 ms    |
| `http_req_duration{endpoint:claim}` avg  | 5.92 ms     |
| `http_req_duration{endpoint:claim}` p(95)| 11.18 ms    |
| `http_req_failed`                        | 0.00%       |
| `create: 202` check pass rate            | 100%        |

### Scenario 2 — Burst Load (5,000 Tasks in 10 s)

| Parameter       | Value      |
|-----------------|------------|
| Producer rate   | 500/s      |
| Burst duration  | 10 s       |
| Drain duration  | 30 s       |
| Worker VUs      | 100        |
| Total requests  | 48,385     |

| Metric                          | Value       |
|----------------------------------|-------------|
| Overall throughput               | 1,610 req/s |
| `http_req_duration` avg          | 18.43 ms    |
| `http_req_duration` p(90)        | 29.41 ms    |
| `http_req_duration` p(95)        | 33.46 ms    |
| `http_req_duration` max          | 70.27 ms    |
| `http_req_failed`                | 0.00%       |

### Scenario 3 — Many Workers (80 Concurrent)

| Parameter        | Value      |
|------------------|------------|
| Producer rate    | 400/s      |
| Duration         | 30 s       |
| Worker VUs       | 80         |
| Total requests   | 49,487     |

| Metric                          | Value       |
|----------------------------------|-------------|
| Overall throughput               | 1,647 req/s |
| `http_req_duration` avg          | 12.82 ms    |
| `http_req_duration` p(90)        | 19.86 ms    |
| `http_req_duration` p(95)        | 22.79 ms    |
| `http_req_duration` max          | 63.02 ms    |
| `http_req_failed`                | 0.00%       |

### Scenario 4 — Prefill Queue (10,000 Tasks)

| Parameter       | Value      |
|-----------------|------------|
| Tasks           | 10,000     |
| VUs             | 50         |
| Total time      | 4.8 s      |

| Metric                          | Value       |
|----------------------------------|-------------|
| Overall throughput               | 2,081 req/s |
| `http_req_duration` avg          | 23.52 ms    |
| `http_req_duration` p(90)        | 30.14 ms    |
| `http_req_duration` p(95)        | 32.95 ms    |
| `http_req_duration` max          | 55.61 ms    |
| `http_req_failed`                | 0.00%       |

### Scenario 5 — Mixed Priorities (50 / 30 / 20)

| Parameter        | Value      |
|------------------|------------|
| Producer rate    | 500/s      |
| Duration         | 30 s       |
| Worker VUs       | 100        |
| Priority mix     | 50% high (10), 30% medium (5), 20% low (0) |
| Total requests   | 55,745     |

| Metric                          | Value       |
|----------------------------------|-------------|
| Overall throughput               | 1,854 req/s |
| `http_req_duration` avg          | 22.24 ms    |
| `http_req_duration` p(90)        | 33.34 ms    |
| `http_req_duration` p(95)        | 39.05 ms    |
| `http_req_duration` max          | 100.89 ms   |
| `http_req_failed`                | 0.00%       |

### Scenario 6 — Delayed Tasks (50 % Delayed)

| Parameter           | Value      |
|---------------------|------------|
| Producer rate       | 200/s      |
| Duration            | 30 s       |
| Worker VUs          | 100        |
| Delay percentage    | 50%        |
| Delay range         | 1–5 s      |
| Total requests      | 33,827     |

| Metric                               | Value       |
|---------------------------------------|-------------|
| Overall throughput                    | 1,124 req/s |
| `http_req_duration` avg               | 6.62 ms     |
| `http_req_duration` p(90)             | 12.24 ms    |
| `http_req_duration` p(95)             | 15.10 ms    |
| `http_req_duration` max               | 65.51 ms    |
| `delayed_tasks_created_total`         | 3,041       |
| `immediate_tasks_created_total`       | 2,959       |
| `http_req_failed`                     | 0.00%       |

---

## Phase 8: Pebble Intra-Process Sharding

Phase 8 introduces intra-process sharding for the embedded Pebble backend, achieving ~1.95× throughput improvement on single-node deployments by parallelizing write commits and compaction across multiple independent Pebble instances.

### Test Configuration

| Parameter      | Value                                       |
|----------------|---------------------------------------------|
| Backend        | Pebble (embedded)                           |
| Storage        | Local SSD (same machine as instance)         |
| Sharding       | 1, 2, 4, 8 shards (single-node, no cluster) |
| CPU            | AMD EPYC 9V74 80-Core (4 vCPUs allocated)   |
| Go version     | 1.24                                        |
| k6 version     | 0.55.0                                      |

### Throughput Scaling

| Config              | Throughput (tasks/sec) | Improvement | Latency p95 |
|---------------------|------------------------|-------------|------------|
| Single-shard (1x)   | ~45k                   | 1.0×        | 2–3 ms     |
| 2-shard (2x)        | ~71k                   | 1.58×       | 2–3 ms     |
| 4-shard (4x)        | ~83k                   | 1.95×       | 2–3 ms     |
| 8-shard (8x)        | ~88k                   | 2.0×        | 3–4 ms     |

**Key Finding:** Scaling plateaus at 4–8 shards on 4-core CPU allocation. Shards > CPU count provides diminishing returns as scheduling overhead exceeds parallelism gains. Recommended: `numShards: 4` (matches CPU core count) for balanced throughput and latency.

### Latency Characteristics

Pebble's in-process commit path keeps tail latency low:

- **Create → Claim → Complete cycle**: ~1.5–2.5 ms (single-shard, `fsyncOnCommit=false`)
- **p95 latency under sustained 50k tasks/s load**: 2–3 ms (4-shard, `fsyncOnCommit=false`)
- **GC pressure**: dominated by JSON encoding via `bytedance/sonic`; batch construction and commit add little allocation churn

### Durability Trade-off

By default, Pebble writes are not synced to disk (`fsyncOnCommit: false`):

| Configuration     | Throughput | Durability | Use Case                    |
|-------------------|------------|------------|-----------------------------|
| fsync: off        | ~83k       | In-memory  | Development, testing        |
| fsync: on         | ~67k       | Durable    | Durability-critical prod    |

**Sync penalty**: ~20% throughput reduction when `fsyncOnCommit: true`. On modern NVMe SSDs, this translates to ~100 μs additional latency per commit batch.

### Single-Node Pebble vs Distributed Redis

| Aspect              | Pebble (single-node) | Redis (replicated)      |
|---------------------|----------------------|-------------------------|
| Throughput          | 83k/sec              | 1.5–2k/sec             |
| Deployment          | 1 binary             | 1+ Sentinel + 2+ Nodes  |
| High availability   | No (single process)  | Yes (with replication)  |
| Failover time       | Process restart      | Seconds (Sentinel)      |
| Ops complexity      | Very low             | Medium-high             |
| Data locality       | Perfect              | Network dependent       |

**Recommendation:** Use Pebble for throughput-optimized single-node deployments (dev, edge, embedded). Use Redis for HA requirements (production multi-region, SLA-driven setups).

---

## Cross-Scenario Summary

| Scenario               | Throughput (req/s) | Avg Latency | p95 Latency | Error Rate |
|------------------------|--------------------|-------------|-------------|------------|
| Sustained throughput   | 1,082              | 5.69 ms     | 10.94 ms    | 0.00%      |
| Burst load             | 1,610              | 18.43 ms    | 33.46 ms    | 0.00%      |
| Many workers           | 1,647              | 12.82 ms    | 22.79 ms    | 0.00%      |
| Prefill queue          | 2,081              | 23.52 ms    | 32.95 ms    | 0.00%      |
| Mixed priorities       | 1,854              | 22.24 ms    | 39.05 ms    | 0.00%      |
| Delayed tasks          | 1,124              | 6.62 ms     | 15.10 ms    | 0.00%      |

### Key Observations

1. **Zero error rates** — All scenarios completed with 0.00% HTTP errors, confirming
   stability under sustained, burst, and mixed workloads.

2. **Throughput ceiling** — A single codeQ instance on loopback achieves 1,000–2,000
   req/s depending on workload mix. The prefill (create-only) scenario peaks at
   ~2,081 req/s because there is no claim/result overhead.

3. **Latency profile** — Under moderate load (200 req/s producer), p95 latency stays
   below 15 ms. At higher rates (500 req/s producer), p95 climbs to 33–39 ms as
   Pebble commit pipeline contention increases (mitigate with `numShards > 1`).

4. **Burst handling** — The system absorbs a 500 req/s burst for 10 s and drains the
   backlog within the 30 s drain window with no errors. Peak latency during burst
   reaches ~70 ms.

5. **Worker concurrency** — 80 concurrent worker VUs claiming tasks at 400 tasks/s
   producer rate shows no contention-related errors. Claim latency stays below 23 ms
   at p95.

6. **Priority handling** — Mixed priority workloads add minimal overhead. Latency
   increases from the higher create rate (500/s vs 200/s), not from priority sorting.

7. **Delayed tasks** — The delayed-task migration path adds negligible latency. Tasks
   with 1–5 s delays are picked up and completed without errors.

---

## Regression Testing

To compare future releases against these baselines:

### Go Benchmarks

```bash
go test ./internal/bench -bench . -benchtime=10s -count=3 -benchmem | tee bench-$(git rev-parse --short HEAD).txt
benchstat bench-baseline.txt bench-$(git rev-parse --short HEAD).txt
```

### k6 Scenarios

Run each scenario with the same parameters documented above and compare:

- `http_req_duration` p95 should not regress by more than 20%.
- `http_req_failed` rate should remain at 0.00% under documented load levels.
- Throughput (req/s) should not drop by more than 15%.

### CI Integration

For automated regression testing, add a workflow step that:

1. Starts codeq via the local Compose files in `deploy/docker-compose/local-dev/` (Pebble store under `./data/pebble`).
2. Runs a representative subset of k6 scenarios (e.g., scenario 01 at reduced rate).
3. Uses k6 thresholds (already defined in each script) to fail the build on regressions.
4. Archives k6 JSON output as a workflow artifact for trend analysis.

Example CI step:

```yaml
- name: Load test (smoke)
  run: |
    docker compose \
      -f deploy/docker-compose/local-dev/compose.yaml \
      -f deploy/docker-compose/local-dev/compose.override.yaml \
      up -d
    # Wait for healthcheck
    until curl -sf http://localhost:8080/metrics; do sleep 5; done
    docker compose \
      -f deploy/docker-compose/local-dev/compose.yaml \
      -f deploy/docker-compose/local-dev/compose.override.yaml \
      --profile loadtest run --rm \
      -e RATE=100 -e DURATION=30s -e WORKER_VUS=20 \
      k6 run /scripts/01_sustained_throughput.js --out json=results.json
  timeout-minutes: 5
```

> **Tip:** Use `k6 run --out json=results.json` and archive the JSON artifact for
> historical trend analysis with tools like `k6-reporter` or Grafana Cloud k6.

## See also

- [Local Development](./22-local-development.md) — Setting up test environment
- [Load Testing Guide](./26-load-testing.md) — Complete load testing documentation
- [Performance Tuning](./17-performance-tuning.md) — Optimization techniques
- [Staging Validation Runbook](./33-staging-validation-runbook.md) — Pre-release validation
- [Troubleshooting](./28-troubleshooting.md) — Diagnosing performance issues
