# Benchmark Regression Testing and CI Integration

## Overview

Automated benchmark regression testing detects performance regressions early by running Go benchmarks on every PR and commit to main. This guide covers setup, interpretation, and best practices.

## Benchmark CI Workflow

### Automated Execution
- **Trigger**: Every push to main and copilot/* branches, every PR to main
- **Scope**: Changes to `**.go`, `internal/bench/**`, `go.mod`, `go.sum`
- **Runtime**: ~2-3 minutes (3 iterations × 10s benchtime)
- **Artifacts**: Results stored for 90 days, history archived for 1 year

### Key Benchmarks

1. **BenchmarkHTTP_CreateClaimComplete**
   - End-to-end HTTP request path: Create → Claim → Complete
   - Baseline: ~2.8ms (3,500-4,000 ops/sec)
   - Measures: Full HTTP stack including Gin routing, auth, JSON marshaling
   - Target: <3ms (1% regression threshold)

2. **BenchmarkScheduler_CreateClaimComplete**  
   - Direct Scheduler interface (no HTTP)
   - Baseline: ~2.7ms (3,600-4,000 ops/sec)
   - Measures: Core scheduling logic with minimal overhead
   - Target: <2.8ms (1% regression threshold)

## Interpreting Results

### Benchmark Output Format
```
BenchmarkHTTP_CreateClaimComplete-4        1226   2886357 ns/op   1961360 B/op   8309 allocs/op
```
- `1226` = iterations completed
- `2886357 ns/op` = nanoseconds per operation (latency)
- `1961360 B/op` = bytes allocated per operation
- `8309 allocs/op` = memory allocations per operation

### Performance Targets
- **Latency (ns/op)**: Should be stable or improve month-over-month
- **Allocations (allocs/op)**: Increasing allocations indicate memory pressure
- **Memory (B/op)**: Total bytes shows GC pressure; reductions are wins

## Regression Detection Strategies

### Manual Review (Current)
1. Compare PR results to main baseline
2. Look for >2% latency increase
3. Identify increasing allocations as performance debt
4. Correlate changes to code modifications

### Recommended Improvements
1. **Automated thresholds**: Fail CI if >5% regression detected
2. **Historical trending**: Track benchmarks over weeks to identify drift
3. **Allocation budgets**: Set per-operation allocation limits
4. **Profiling integration**: Link slow benchmarks to CPU/memory profiles

## Running Benchmarks Locally

### Quick run (default)
```bash
go test -bench=. -benchmem ./internal/bench
```

### Production-like (10s iterations, 3 runs for stability)
```bash
go test -bench=. -benchmem -benchtime=10s -count=3 ./internal/bench
```

### With CPU profiling
```bash
go test -bench=. -benchmem -cpuprofile=cpu.prof ./internal/bench
go tool pprof cpu.prof
```

## Optimization Workflow

1. **Establish baseline**: Run benchmarks 3× before changes
2. **Implement change**: Focus on high-impact paths (claim, result operations)
3. **Verify improvement**: Run benchmarks 3× to check variance
4. **Commit results**: Include before/after metrics in PR description
5. **Monitor CI**: Watch automatic regressions after merge

## Common Optimization Targets

### Claim Path (Hot)
- Baseline: 300-400 µs for SETEX + HSET + response
- Optimization: Redis pipelining (2 RTTs → 1 RTT = 50% latency)
- Measurement: Count Redis commands in pprof graph

### Result Operations
- Baseline: 200-300 µs for updates + cleanup
- Optimization: Batch Redis operations
- Measurement: Memory allocations should decrease

### JSON Serialization  
- Current: Sonic codec (5-15% faster than encoding/json)
- Bottleneck: Large payloads with nested objects
- Measurement: Profile allocation hotspots with pprof

## CI Integration Points

- **On every PR**: Automatic benchmark execution, baseline comparison available
- **On main commits**: Historical tracking enabled, regression alerts enabled
- **On release tags**: Detailed benchmark report with improvement summaries

## Future Enhancements

- Benchmark threshold enforcement (fail CI on >5% regression)
- Historical trend analysis (alert on 10% monthly drift)
- Load test integration (k6 scenarios with performance targets)
- Memory profiling dashboard
- Comparative benchmarking (main vs PR side-by-side)
