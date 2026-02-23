# Profiling & Diagnostics Guide

## Quick Diagnostics for Performance Issues

### CPU Profiling
Identify hot paths and expensive operations during load testing:

```bash
# Run benchmarks with CPU profiling
go test -bench=BenchmarkHTTP_CreateClaimComplete \
  -cpuprofile=cpu.prof ./internal/bench

# Analyze the profile
go tool pprof -http=localhost:8080 cpu.prof
```

Navigate using:
- **Graph view**: See call tree with cumulative time
- **Top** list: Sort by cumulative time to find bottlenecks
- **Source** view: See exact line-by-line timings

### Memory Profiling
Find memory leaks and allocation hotspots:

```bash
# Heap allocation profile
go test -bench=BenchmarkScheduler_CreateClaimComplete \
  -memprofile=mem.prof -benchtime=10s ./internal/bench

# Analyze allocations
go tool pprof -alloc_space mem.prof  # Total allocated
go tool pprof -alloc_objects mem.prof # Allocation count
go tool pprof -inuse_space mem.prof  # Live memory
```

## Runtime Profiling in Production

Enable pprof via import (already done if using OpenTelemetry):

```go
import _ "net/http/pprof"
```

Collect profiles from running server:

```bash
# 30-second CPU profile
curl http://localhost:6060/debug/pprof/profile?seconds=30 > cpu.prof

# Heap snapshot
curl http://localhost:6060/debug/pprof/heap > heap.prof
```

## Flame Graphs

For visual analysis, convert profiles to flame graphs:

```bash
# Install flamegraph tools
go install github.com/google/pprof@latest

# Generate SVG
go tool pprof -svg cpu.prof > cpu.svg
```

## Key Metrics to Watch

- **Allocation rate** (alloc_objects): High rate means garbage collector overhead
- **Goroutine count**: Unbounded growth indicates leaks
- **Heap size trend**: Should stabilize under sustained load
- **Lock contention** (if using mutexes): Look for lock/unlock in hot paths
