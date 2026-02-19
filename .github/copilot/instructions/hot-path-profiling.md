# Hot Path Profiling Guide

## Quick Profiling Loop

Identify performance bottlenecks in codeQ using Go's built-in profiling tools. This guide enables engineers to measure impact in minutes.

### Step 1: Run Benchmark with Profile

```bash
# Generate CPU and memory profiles for the claim operation (hottest path)
go test -cpuprofile=cpu.prof -memprofile=mem.prof \
  -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s ./internal/bench
```

### Step 2: Analyze CPU Profile

```bash
# Show top 10 CPU-consuming functions
go tool pprof -text -top -nodecount=10 cpu.prof

# Interactive analysis (type 'list FunctionName' to see source lines)
go tool pprof cpu.prof
```

**What to look for:**
- Functions consuming >10% CPU
- Unexpected allocations in hot paths
- Repeated operations that could be batched

### Step 3: Analyze Memory Profile

```bash
# Show allocation patterns (space used)
go tool pprof -text -top -alloc_space mem.prof | head -20

# Show allocation counts (number of allocations)
go tool pprof -text -top -alloc_objects mem.prof | head -20

# Show in-use memory (memory still allocated)
go tool pprof -text -top -inuse_space mem.prof | head -20
```

**What to look for:**
- Functions allocating >5% of memory
- Repeated small allocations that could use object pools
- Temporary allocations that could be reused

## Focus Areas for codeQ

### 1. Claim Path (P99 latency critical)
- Measure: Task lookup → lease assignment → response
- Target: < 5ms P99 under load
- Profile commands:

```bash
# 30-second profile for statistical significance
go test -cpuprofile=cpu.prof -memprofile=mem.prof \
  -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=30s ./internal/bench
```

### 2. Redis Pipeline Efficiency
- Measure: Redis command grouping and latency
- Use `-trace` flag for detailed timing:

```bash
go test -trace=trace.out -bench=Benchmark \
  -benchtime=5s ./internal/bench
go tool trace trace.out
```

### 3. JSON Unmarshaling
- Profile memory allocations in task/result handling
- Look for repeated `json.Unmarshal` in profiles
- Consider `sonic` (already imported) for hot paths

## Before/After Comparison

```bash
# Baseline (before optimization)
go test -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s -benchmem ./internal/bench > baseline.txt

# After optimization
go test -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s -benchmem ./internal/bench > optimized.txt

# Compare (benchmark stats tool or manual inspection)
diff baseline.txt optimized.txt
```

## Key Metrics

- **Operations/sec**: Higher is better (increased throughput)
- **ns/op**: Lower is better (latency per operation)
- **B/op**: Lower is better (memory per operation)
- **Allocs/op**: Lower is better (allocation count)

## Profiling Tips

1. **Warm up before profiling**: Run benchmarks multiple times to allow JIT/GC to stabilize
2. **Use realistic data**: Ensure benchmark data (task size, Redis depth) matches production patterns
3. **Profile under load**: Single-threaded benchmarks miss contention issues
4. **Compare across commits**: Track regressions automatically

## Integration with Load Tests

For realistic profiling, combine benchmarks with k6 load tests:

```bash
# Terminal 1: Start codeQ
docker compose up

# Terminal 2: Run k6 with pprof
# Add ?debug=true to codeQ server logs
docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Terminal 3: Profile while k6 runs
go tool pprof -http=:8081 http://localhost:6060/debug/pprof/profile?seconds=30
```
