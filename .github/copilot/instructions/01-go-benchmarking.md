# Go Benchmarking Guide for CodeQ

## Overview

Go benchmarks in CodeQ run fully in-process using `miniredis` (Redis mock) and Gin HTTP test server. They're designed for fast regression detection—typically 5-30 seconds—making them ideal for iterative performance engineering.

## Key Files

- `internal/bench/http_bench_test.go`: Full HTTP workflow benchmarks (create → claim → complete)
- Run with: `go test ./internal/bench -bench . -benchtime=5s -benchmem`

## Running Benchmarks

### Basic run (all benchmarks, 5 seconds each)
```bash
go test ./internal/bench -bench . -benchtime=5s -benchmem
```

### Specific benchmark with detailed timing
```bash
go test ./internal/bench -bench BenchmarkHTTP_CreateClaimComplete -benchtime=10s -benchmem -v
```

### With CPU profiling (heavy analysis)
```bash
go test ./internal/bench -bench . -benchtime=10s -cpuprofile=cpu.prof -memprofile=mem.prof
go tool pprof cpu.prof
```

## Benchmark Composition

Each benchmark:
1. Initializes miniredis + HTTP server (one-time setup)
2. Measures N iterations of the workflow
3. Reports ns/op, B/op (bytes allocated), allocs/op

**Example interpretation:**
```
BenchmarkFullWorkflow-12    1000    1234567 ns/op    45678 B/op    123 allocs/op
                                     ↓ nanoseconds    ↓ bytes       ↓ allocations
                                   ~1.2 ms/op       ~45 KB/op
```

## Regression Detection Strategy

1. **Establish baseline**: Run benchmark on main branch
   ```bash
   go test ./internal/bench -bench . -benchtime=10s > main-baseline.txt
   ```

2. **Test your changes**: Run on your branch
   ```bash
   go test ./internal/bench -bench . -benchtime=10s > feature-branch.txt
   ```

3. **Compare with benchstat** (if available)
   ```bash
   benchstat main-baseline.txt feature-branch.txt
   ```
   Or manually: compare ns/op and B/op columns

4. **Accept if**: Latency unchanged or faster; allocation unchanged or lower

## Common Pitfalls

- **Too short benchtime**: Default is 1s; use -benchtime=10s+ for stable results
- **System noise**: Close background apps; results vary ±5-10% on local machines
- **Miniredis vs real Redis**: Miniredis has no network overhead; use k6 for realistic latency
- **Memory allocations**: High allocs/op may indicate unnecessary object creation

## Next Steps for Large Changes

If benchmark shows >10% regression:
- Use CPU profiling to identify hot spots
- Check memory allocation patterns (pprof -alloc_space)
- Consider k6 load tests for sustained load impact
