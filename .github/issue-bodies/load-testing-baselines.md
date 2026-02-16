## Context
The load testing framework was just merged (PR #153) with k6-based benchmarks in `loadtest/k6/`. The daily status report encourages contributors to "Try the Load Testing Framework - Run the new k6 benchmarks and share results."

## Objective
Establish baseline performance metrics and create a central place to collect and track load testing results from different environments.

## Scope

### Baseline Scenarios to Run
Using the k6 scripts in `loadtest/k6/`:
- [ ] `01_sustained_throughput.js` - Sustained load baseline
- [ ] `02_burst_10k_10s.js` - Burst handling capacity
- [ ] `03_many_workers.js` - Worker concurrency limits
- [ ] `04_prefill_queue.js` - Large queue depth behavior
- [ ] `05_mixed_priorities.js` - Priority queue performance
- [ ] `06_delayed_tasks.js` - Delayed task scheduling overhead

### Test Environments
- [ ] Local development (docker-compose)
- [ ] CI/CD pipeline (GitHub Actions)
- [ ] Staging environment (if available)
- [ ] Production-like load testing environment

### Metrics to Collect
For each scenario, capture:
- Request rates (req/s)
- Latency percentiles (p50, p95, p99)
- Error rates
- Queue depth metrics
- Resource utilization (CPU, memory, disk I/O)
- KVRocks/Redis metrics

### Deliverables
1. Baseline results document in `docs/load-testing-baselines.md`
2. Instructions for running standardized tests
3. Template for reporting results
4. Comparison table across environments

## How to Contribute
Contributors can help by:
1. Running the load tests in their environment
2. Reporting results in this issue (use the template below)
3. Comparing results against different configurations
4. Identifying performance bottlenecks

### Results Template
```markdown
### Environment
- OS: 
- CPU: 
- Memory: 
- KVRocks version: 
- codeQ version: 

### Test: [scenario name]
- Command: `docker compose --profile loadtest run --rm k6 run /scripts/XX_scenario.js`
- Duration: 
- Results:
  - Requests/sec: 
  - p50 latency: 
  - p95 latency: 
  - p99 latency: 
  - Error rate: 
  - Notes: 
```

## Success Criteria
- Baseline results documented for all 6 scenarios
- At least 3 different environment configurations tested
- Performance targets established for future optimization
- Regression detection criteria defined

## Related
- PR #153: Load testing framework and benchmarks
- PR #157: Load testing cross-references
- Issue #30: Load testing harness (closed/merged)
- `docs/26-load-testing.md`: Load testing documentation
- Daily Status Report: "Try the Load Testing Framework - Run the new k6 benchmarks and share results"

---
*Created to track load testing results as recommended in the daily status report*
