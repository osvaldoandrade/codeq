# Hot Path Profiling and Performance Analysis

## Quick Start
Profile hot paths efficiently using Go's built-in pprof tool integrated with the build-steps workflow. Identifies CPU, memory, and concurrency bottlenecks.

## Profiling Setup

### 1. CPU Profiling
```bash
# Add to your benchmark test or run server with CPU profile
go test -bench=. -benchcpu -cpuprofile=cpu.prof ./internal/bench
go tool pprof cpu.prof

# Interactive commands in pprof:
# top       - Show top functions by CPU time
# list      - Show source code for a function
# web       - Generate SVG graph (requires graphviz)
```

### 2. Memory Profiling
```bash
# Memory allocations and heap usage
go test -bench=. -benchmem -memprofile=mem.prof ./internal/bench
go tool pprof mem.prof

# Interactive commands:
# top       - Show functions by memory allocs
# list      - Show source code with alloc counts
# alloc_space - Total alloc (not freed)
# inuse_space - Currently in use
```

### 3. Finding Hot Paths
**Focus on task lifecycle first:**
- Task claim operations (P99 ≤ 100ms target)
- Task result processing
- Queue introspection

**Identify bottlenecks:**
1. Check allocation patterns (`-benchmem`): High alloc count = inefficiency
2. Profile CPU (`-cpuprofile`): Identify expensive functions
3. Trace locks (`pprof` goroutine graph): Contention points

## Common Optimizations

### Reduce Allocations
```go
// ❌ Bad: Creates slice per operation
func ProcessTask(tasks []Task) {
    for _, t := range tasks {
        results := make([]byte, 1024) // Allocated every loop
    }
}

// ✅ Good: Reuse buffer (pool or stack)
func ProcessTask(tasks []Task) {
    buf := make([]byte, 1024) // Allocate once
    for _, t := range tasks {
        // Reuse buf
    }
}
```

### Use Object Pooling for Frequent Allocations
```go
var bufferPool = sync.Pool{
    New: func() interface{} {
        return make([]byte, 4096)
    },
}

buf := bufferPool.Get().([]byte)
defer bufferPool.Put(buf)
```

### GC Pressure Monitoring
```bash
# Run benchmark with GC stats
GODEBUG=gctrace=1 go test -bench=. -benchmem ./internal/bench
# Look for GC frequency and pause time
```

## Real-world Example: Task Claim Optimization
1. Profile claim operation: `go test -bench=BenchmarkClaim -cpuprofile=claim.prof`
2. Check pprof `top` for hot functions (likely Redis operations)
3. Check allocations: `go tool pprof -alloc_space claim.prof`
4. Identify repeated unmarshaling or encoding
5. Implement caching or batch operations
6. Re-profile to verify improvement

## Success Metrics
- ✅ Allocation count reduced in hot paths
- ✅ GC pause time < 100ms under sustained load
- ✅ CPU profile shows even distribution (no single hotspot > 40%)
- ✅ Benchmarks show improvement with lower variance
