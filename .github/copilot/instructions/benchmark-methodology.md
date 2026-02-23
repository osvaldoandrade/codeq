# Benchmark Methodology Guide

## Running Benchmarks in codeQ

### Local Benchmark Execution

```bash
# Run all benchmarks once
go test -bench=. ./internal/bench

# Run specific benchmark 10 times for stable results
go test -bench=BenchmarkHTTP_CreateClaimComplete \
  -benchmem -benchtime=3s -count=10 ./internal/bench

# Get allocation statistics
go test -bench=. -benchmem ./internal/bench | grep "Alloc"
```

### Understanding Output

```
BenchmarkHTTP_CreateClaimComplete-12    42532    28156 ns/op    3567 B/op    23 allocs/op
                                          ↓       ↓           ↓           ↓
                                    iterations  time/op   bytes/op   allocs/op
```

- `iterations`: Number of times the benchmark ran (higher = more stable)
- `ns/op`: Nanoseconds per operation (this is what you optimize)
- `B/op`: Bytes allocated per operation
- `allocs/op`: Number of allocations per operation

## Baseline Establishment

1. **Create baseline on clean code:**
   ```bash
   git stash
   go test -bench=. -count=5 ./internal/bench > baseline.txt
   ```

2. **Run after changes:**
   ```bash
   go test -bench=. -count=5 ./internal/bench > modified.txt
   ```

3. **Compare using benchstat:**
   ```bash
   go install golang.org/x/perf/cmd/benchstat@latest
   benchstat baseline.txt modified.txt
   ```

## Interpreting Comparison Results

```
name                           old time/op    new time/op    delta
BenchmarkHTTP_CreateClaimComplete    28.2µs ± 3%    26.1µs ± 2%  -7.4%  ✓

name                           old alloc/op   new alloc/op   delta
BenchmarkHTTP_CreateClaimComplete     3567 B ±0%     3200 B ±0% -10.3%  ✓
```

- `±3%`: Statistical confidence interval (lower is better - more stable)
- `delta`: Percentage change (negative = improvement)
- Benchstat marks significant changes with `✓` or `!`

## Avoiding False Positives

- **Use -count=5 or more** to get statistical significance
- **Isolate from system load**: Stop other processes before running
- **Use `-benchtime=Xs`** to run longer for unstable benchmarks
- **Test on same hardware**: Results vary between machines
- **Watch for GC pauses**: Use `-benchtime=10s` to span multiple GC cycles

## Performance Regression Threshold

In codeQ:
- **Latency**: Reject changes that increase p99 latency >5%
- **Allocations**: Aim to reduce allocations in hot paths
- **Memory**: Watch heap growth under sustained load (100k+ operations)
