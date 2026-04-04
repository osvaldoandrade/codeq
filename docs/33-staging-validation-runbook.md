# 33 — Staging Validation Runbook

This runbook describes how to validate codeQ performance in a staging environment, define acceptable ranges, and track regressions over time.

## 1. Prerequisites

| Requirement | Details |
|---|---|
| Staging cluster | Docker Compose or Kubernetes deployment matching production topology |
| k6 | v0.55+ (`docker compose --profile loadtest`) |
| Go toolchain | 1.24+ for running Go benchmarks |
| `benchstat` | `go install golang.org/x/perf/cmd/benchstat@latest` |
| Monitoring | Prometheus + Grafana (optional but recommended) |

## 2. Go Benchmark Validation

### 2.1 Run Benchmarks

From the repository root on the staging host (or any machine with Go installed):

````bash
# Full benchmark suite (Sonic codec, GC pressure, HTTP, Scheduler)
go test ./internal/bench -bench=. -benchtime=10s -count=5 -benchmem \
  | tee bench-staging-$(date +%Y%m%d).txt

# Bloom filter benchmarks (in-process, no Redis needed)
go test ./internal/repository -bench=BenchmarkBloom -run='^$' \
  -benchtime=10s -count=5 -benchmem \
  | tee bloom-staging-$(date +%Y%m%d).txt
````

### 2.2 Compare Against Baselines

````bash
# Compare full benchmarks
benchstat docs/bench-baseline.txt bench-staging-$(date +%Y%m%d).txt

# Compare Bloom filter benchmarks
benchstat docs/bloom-baseline.txt bloom-staging-$(date +%Y%m%d).txt
````

### 2.3 Acceptable Ranges

| Benchmark | Baseline | Acceptable Range | Regression Threshold |
|---|---|---|---|
| `BenchmarkHTTP_CreateClaimComplete` | ~3.2 ms/op | ≤ 3.5 ms/op | > 10% slower |
| `BenchmarkScheduler_CreateClaimComplete` | ~3.1 ms/op | ≤ 3.4 ms/op | > 10% slower |
| `BenchmarkSonic_MarshalTask_Medium` | (establish on first run) | ≤ 1.2× baseline | > 20% slower |
| `BenchmarkSonic_UnmarshalTask_Medium` | (establish on first run) | ≤ 1.2× baseline | > 20% slower |
| `BenchmarkBloom_Add` | (establish on first run) | ≤ 1.2× baseline | > 20% slower |
| `BenchmarkBloom_MaybeHas_Negative` | (establish on first run) | ≤ 1.2× baseline | > 20% slower |
| `BenchmarkGCPressure_SustainedEnqueue` | (establish on first run) | allocs/op ≤ 1.1× baseline | > 10% more allocs |

## 3. Sonic Codec Validation

The Sonic codec (`github.com/bytedance/sonic`) is used on all hot paths (task, result, and subscription repositories). To validate it outperforms `encoding/json`:

````bash
go test ./internal/bench -bench='Benchmark(Sonic|StdJSON)' -benchtime=10s \
  -count=5 -benchmem | tee sonic-comparison-$(date +%Y%m%d).txt
````

**Expected improvements over `encoding/json`:**

| Metric | Expected Improvement |
|---|---|
| Marshal ns/op | 2–3× faster |
| Unmarshal ns/op | 2–3× faster |
| Marshal B/op | 30–50% fewer |
| Unmarshal allocs/op | 40–50% fewer |

If Sonic shows less than 1.5× improvement on marshal or unmarshal, investigate whether the Sonic JIT compiler is active (requires `amd64` and `CGO_ENABLED=1`).

## 4. Bloom Filter Validation

### 4.1 False Positive Rate

````bash
go test ./internal/repository -run=TestBloom_FalsePositiveRate -v
````

Expected output: observed FP rate ≤ 2% (2× the 1% target to allow for statistical variance). If the observed rate exceeds 2%, check that:
- Capacity is set to 1,000,000 (default)
- Rotation period has not been reduced below 30 minutes

### 4.2 Memory Footprint

````bash
go test ./internal/repository -run=TestBloom_MemorySizeConsistency -v
````

Expected memory usage per filter:

| Capacity | FP Rate | Approximate Memory |
|---|---|---|
| 100,000 | 1% | ~117 KB |
| 1,000,000 | 1% | ~1.14 MB |
| 2,000,000 | 1e-12 | ~92 MB |

## 5. k6 Load Test Validation

### 5.1 Start Staging Environment

````bash
docker compose up -d
# Wait for healthcheck
until curl -sf http://localhost:8080/metrics; do sleep 5; done
````

### 5.2 Run Scenarios

Run each scenario and compare against the baselines in `docs/30-performance-baselines.md`:

````bash
# Scenario 1: Sustained throughput (most representative)
docker compose --profile loadtest run --rm \
  k6 run /scripts/01_sustained_throughput.js --out json=results-01.json

# Scenario 2: Burst load
docker compose --profile loadtest run --rm \
  k6 run /scripts/02_burst_10k_10s.js --out json=results-02.json

# Scenario 3: Many workers
docker compose --profile loadtest run --rm \
  k6 run /scripts/03_many_workers.js --out json=results-03.json

# Scenario 4: Prefill queue
docker compose --profile loadtest run --rm \
  k6 run /scripts/04_prefill_queue.js --out json=results-04.json

# Scenario 5: Mixed priorities
docker compose --profile loadtest run --rm \
  k6 run /scripts/05_mixed_priorities.js --out json=results-05.json

# Scenario 6: Delayed tasks
docker compose --profile loadtest run --rm \
  k6 run /scripts/06_delayed_tasks.js --out json=results-06.json
````

### 5.3 Acceptable Ranges

| Metric | Threshold | Action |
|---|---|---|
| `http_req_failed` rate | Must be 0.00% | **P0** — investigate immediately |
| `http_req_duration` p95 | ≤ 1.2× baseline | Review for regressions |
| `http_req_duration` p99 | ≤ 1.5× baseline | Review for tail latency issues |
| Throughput (req/s) | ≥ 0.85× baseline | Review for capacity regressions |

## 6. GC Pressure Monitoring

### 6.1 Benchmark-Based Monitoring

````bash
go test ./internal/bench -bench='BenchmarkGCPressure' -benchtime=10s \
  -count=5 -benchmem | tee gc-staging-$(date +%Y%m%d).txt
````

The benchmarks report custom metrics:
- `gc_cycles`: Number of GC cycles during the benchmark
- `gc_pause_ms`: Total GC pause time in milliseconds

### 6.2 Runtime Monitoring

During k6 load tests, monitor GC metrics via the `/metrics` endpoint:

````bash
# Watch GC metrics in real time during a load test
watch -n5 'curl -s http://localhost:8080/metrics | grep go_gc'
````

Key Prometheus metrics to watch:

| Metric | Healthy Range |
|---|---|
| `go_gc_duration_seconds{quantile="1"}` | < 10 ms |
| `go_memstats_heap_alloc_bytes` | Stable (not monotonically increasing) |
| `go_goroutines` | Stable under constant load |

## 7. Performance Dashboard

### 7.1 CI-Based Tracking (Default)

The `benchmark-regression.yml` workflow automatically:
1. Runs all Go benchmarks on every PR and push to `main`
2. Archives results as workflow artifacts (90-day retention, 1-year history)
3. Compares PR benchmarks against the `main` branch baseline
4. Posts regression analysis to the GitHub workflow step summary

To review historical trends, download artifacts from the **Actions** tab:
- `benchmark-results` — Individual run outputs
- `benchmark-history` — Time-series data for trend analysis

### 7.2 Grafana Dashboard (Optional)

For teams with Prometheus + Grafana:

1. Import the codeQ runtime metrics from `/metrics`
2. Create panels for:
   - **Request latency** (p50, p95, p99) from `http_request_duration_seconds`
   - **Throughput** from `http_requests_total` rate
   - **GC pause times** from `go_gc_duration_seconds`
   - **Heap usage** from `go_memstats_heap_alloc_bytes`
   - **Goroutine count** from `go_goroutines`
3. Set alerts for:
   - p99 latency > 2× baseline
   - Error rate > 0.1%
   - Heap growth > 50% over 1 hour (possible memory leak)

## 8. Pre-Release Checklist

Before each release, validate all of the following in staging:

- [ ] Go benchmarks show no regression > 10% vs baseline
- [ ] Sonic codec outperforms `encoding/json` by at least 1.5×
- [ ] Bloom filter false positive rate ≤ 2%
- [ ] k6 scenarios pass with 0% error rate
- [ ] k6 p95 latencies within 1.2× of baseline
- [ ] GC pause times < 10 ms under sustained load
- [ ] No goroutine leaks under 5-minute sustained load test

## References

- Performance baselines: `docs/30-performance-baselines.md`
- Load testing guide: `docs/26-load-testing.md`
- Performance tuning: `docs/17-performance-tuning.md`
- Benchmark CI workflow: `.github/workflows/benchmark-regression.yml`
- Benchmark CI guide: `.github/copilot/instructions/07-benchmark-regression-ci.md`
