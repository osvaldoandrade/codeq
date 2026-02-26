# Profiling and Bottleneck Identification Guide for CodeQ

## Overview

When benchmarks or load tests show performance issues, profiling pinpoints the exact cause. Use CPU profiling for latency, memory profiling for allocations, and Go's execution tracer for concurrency issues.

## CPU Profiling (Latency Bottlenecks)

### Run benchmarks with CPU profile
```bash
go test ./internal/bench -bench BenchmarkFullWorkflow -benchtime=10s -cpuprofile=cpu.prof
go tool pprof cpu.prof
```

### Inside pprof interactive shell
```
(pprof) top10         # Top 10 functions by CPU time
(pprof) list Claim    # Show source code for Claim function
(pprof) web           # Generate flamegraph (requires graphviz)
```

**What to look for**: Functions consuming >5% CPU time are worth optimizing. Most time should be in Redis operations or Lua script execution.

### Interpret CPU profile
- **High time in Lua scripts**: Consider probabilistic repair or delayed TTL checks (see HLD #24)
- **High time in JSON unmarshaling**: Consider codec optimization (structured binary format)
- **High time in lock contention**: Look for RWMutex hotspots; consider lock-free structures

## Memory Profiling (Allocation Bottlenecks)

### Run with memory allocation profile
```bash
go test ./internal/bench -bench . -benchtime=10s -memprofile=mem.prof -allocmem=<size>
go tool pprof -alloc_space mem.prof   # All allocations (total bytes)
go tool pprof -alloc_objects mem.prof # Allocation count
go tool pprof -inuse_space mem.prof   # In-use memory (RSS)
```

### Reduce unnecessary allocations
```bash
# Compare allocations before/after
go test ./internal/bench -bench . -benchmem
```

**Typical optimization targets**:
- `json.Unmarshal`: Consider protobuf or schema-based codecs
- `[]byte` copies: Use io.Reader interfaces instead of buffering
- Task result builders: Pre-allocate slices if size known

## Goroutine Profiling (Concurrency Issues)

### Detect goroutine leaks
```bash
go test ./internal/backoff -run TestLeaks -v
```

### Trace goroutine spawning
```bash
go test ./internal/bench -bench . -benchtime=5s -trace=trace.out
go tool trace trace.out
```
Open in browser; look for:
- Excessive goroutine creation (should be bounded)
- Blocking channels (blue bars in timeline)
- GC pauses (pink bands)

## Real-Time Profiling (Running Server)

### Enable pprof on server
```bash
# Server has pprof endpoint at localhost:6060 (if profiling enabled)
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

### Profile load test while running
1. Start codeQ: `docker compose up -d`
2. In another terminal, start CPU profile collection
3. Run k6 scenario: `docker compose --profile loadtest run ...`
4. Analyze profile: `go tool pprof cpu.prof`

## Bottleneck Detection Strategy

### 1. Quick wins (CPU < 1%)
- Unnecessary string concatenations (use strings.Builder)
- Repeated struct allocations (use object pool)
- File I/O in hot path (use buffering)

### 2. Moderate improvements (1-5% CPU)
- O(N) loops that could be O(log N) (use sorted structures)
- Redis round-trips that could be pipelined (bundle operations)
- Inefficient JSON marshaling (validate schema, consider protobuf)

### 3. Major optimizations (>5% CPU)
- Algorithm changes (e.g., claim repair strategy; HLD #24)
- Concurrency improvements (sharding; HLD #24)
- System-level changes (batching, caching, indexing)

## Common Bottlenecks and Fixes

| Symptom | Root Cause | Fix |
|---------|-----------|-----|
| Claim latency spikes at 100K+ queue depth | O(N) Lua iteration | Probabilistic repair; see HLD #24 |
| High allocations (>1KB/op) | JSON marshaling | Consider binary format; validate schema |
| CPU saturation <4K ops/sec | Single Redis instance | Implement sharding; see HLD #24 |
| Memory grows unbounded | In-progress queue never cleaned | Add background cleanup service |

## Measurement Validation

Always validate measurements are reproducible:
1. Run baseline twice, expect ±5% variation
2. Run on same hardware (or same VM)
3. Close background processes
4. For accurate latency, use same environment (docker-compose)

Measurements off by 10%+ indicate system noise; increase benchtime or sample size.
