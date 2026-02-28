# Guide 5: Code-Level Optimization Patterns

## This Codebase's Optimization Hotspots

### 1. Task Claim Latency (High Priority)

**Location:** `internal/repository/task_repository.go` - `ClaimTask()` method

**Current approach:**
- O(1) queue operations using KVRocks/RocksDB pipelined requests
- Lease repair handled separately with exponential backoff
- Bloom filters for idempotency, ghost tasks, cleanup deduplication

**Optimization opportunities:**
```go
// Profile this path under heavy concurrent load
// Measure with: go test -bench . -benchtime=30s ./internal/bench
// Look for:
// - Lock contention on lease repair
// - Memory allocations in request marshaling
// - KVRocks operation latency
```

**Measurement before change:**
```bash
go test -bench Claim -benchtime=10s ./internal/bench > before.txt
k6 run --duration=30s loadtest/k6/01_sustained_throughput.js
# Note p99 claim latency from k6 output
```

### 2. Memory Pressure (Medium Priority)

**Location:** `pkg/persistence/memory/plugin.go` - default for tests/dev

**Issue:** 
- No eviction policy; memory grows unbounded
- Suitable for testing only; not production

**For optimization:**
- Use miniredis in benchmarks (already done in internal/bench)
- Profile heap growth under sustained load
- Target: < 500MB heap under 1min continuous load

### 3. Rate Limiting CPU (Medium Priority)

**Location:** `internal/ratelimit/token_bucket.go`

**Pattern:**
- Per-tenant token bucket implementation
- Called on every request (hot path)
- Minimize allocations here

**Quick wins:**
```go
// Check if token bucket uses sync.Mutex or sync/atomic
// Profiling shows if high CPU in Lock/Unlock?
// Consider: pre-allocating request token objects
```

### 4. Repository Layer Caching (Low Priority)

**Location:** All `*Repository` interfaces in `internal/repository/`

**Pattern observed:**
- Results cached in interfaces (good)
- Consider: shared cache across repositories for frequently accessed tenant config

**Measurement:**
```bash
# Benchmark with many concurrent readers
go test -bench . -benchtime=30s -parallel=8 ./internal/repository
# Compare baseline vs. with caching
```

## General Optimization Process for Go

1. **Measure before:** Establish baseline with benchmarks or k6
2. **Profile:** Identify hotspot with CPU or memory profiler
3. **Implement:** Make targeted change to hot function
4. **Measure after:** Run same benchmark/test, compare results
5. **Validate:** Ensure no regression in other metrics

Example:
```bash
# Step 1: Baseline
go test -bench ClaimTask -benchtime=10s -cpuprofile=cpu_before.prof ./internal/bench
go tool pprof -top cpu_before.prof > profile_before.txt

# Step 2: Make code change
# vi internal/repository/task_repository.go

# Step 3: Recompile and retest
go test -bench ClaimTask -benchtime=10s -cpuprofile=cpu_after.prof ./internal/bench

# Step 4: Compare
go tool pprof -top cpu_after.prof > profile_after.txt
diff profile_before.txt profile_after.txt
```

## Avoiding Common Pitfalls

**Don't:**
- ❌ Optimize without measuring (you might make it slower)
- ❌ Chase micro-optimizations in cold code paths
- ❌ Trade readability for 1% gains in non-critical paths
- ❌ Assume benchmark = real-world behavior (run k6 load tests too)

**Do:**
- ✓ Profile under realistic load (use k6 scenarios)
- ✓ Make changes to top 5% hottest functions
- ✓ Always compare before/after with same conditions
- ✓ Document why optimization was needed (link to profile)
- ✓ Add benchmark regression test if important

## Documentation References

- **Go profiling:** `go help test` (see -cpuprofile flag)
- **Codebase testing:** `docs/26-load-testing.md`, `docs/17-performance-tuning.md`
- **KVRocks tuning:** `docs/17-performance-tuning.md` section on RocksDB config
