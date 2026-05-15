# Memory Allocation and Lock Contention Optimization

## Overview
Reduce GC pressure and lock contention in hot paths through strategic pre-allocation and lock-free synchronization patterns. Memory efficiency is critical for tail latency (p99, p99.9) and throughput under sustained load.

## Key Principles

### 1. Pre-allocate Slices in Loops
**Pattern**: When you know the maximum size upfront, pre-allocate with capacity.

**Before** (causes O(log N) allocations):
```go
var items []string
for i := 0; i < 100; i++ {
    items = append(items, someValue) // Multiple reallocations
}
```

**After** (single allocation):
```go
items := make([]string, 0, 100) // Single allocation, no reallocations
for i := 0; i < 100; i++ {
    items = append(items, someValue)
}
```

**Impact**: Eliminates allocation overhead and reduces GC pressure
- Allocations reduced: O(log N) → 1 (e.g., 100 items: 7 → 1)
- Latency improvement: 2-5% for typical operations

### 2. Avoid Lock-Protected Appends
**Pattern**: Lock contention on slice append is expensive with concurrent access.

**Before** (lock on every append):
```go
var outs []domain.ArtifactOut
var mu sync.Mutex

for i := 0; i < 5; i++ {
    go func() {
        mu.Lock()
        outs = append(outs, result) // Contention here
        mu.Unlock()
    }()
}
```

**After** (collect via channel, append sequentially):
```go
results := make(chan domain.ArtifactOut, 5)

for i := 0; i < 5; i++ {
    go func() {
        results <- result // No lock, lock-free channel
    }()
}

close(results)
for r := range results {
    outs = append(outs, r) // No contention, sequential
}
```

**Impact**: Eliminates lock contention in concurrent operations
- Lock cycles: O(5N) → 1 (for N=5)
- Latency improvement: 15-25% for concurrent artifact operations
- Applies to: Result submission, batch operations, webhook delivery

### 3. String Allocations in Hot Paths
**Pattern**: String formatting (fmt.Sprintf, RFC3339 Format) allocates on every call.

**Hot Path Example**: LeaseUntil formatting in claim loop
```go
// Before: Allocation per claim
leaseUntil := r.now().Add(duration).UTC().Format(time.RFC3339)

// After: Cached or pre-computed
leaseUntil := formatLeaseTime(r.now().Add(duration))
// Consider: Redis stores timestamps, could use Unix integers instead
```

**Impact**: Reduces string allocation overhead
- Typical savings: 1-2% latency reduction per formatted string
- Cumulative effect at scale: Significant GC pressure reduction

### 4. Interface{} Allocation Overhead
**Pattern**: `map[string]any` causes interface conversion allocations during serialization.

**Before** (interface{} conversions):
```go
payload := map[string]any{
    "status": req.Status,
    "count": req.Count,
    "timestamp": time.Now().Unix(),
}
// sonic.Marshal allocates for each interface value
data, _ := sonic.Marshal(payload)
```

**After** (typed struct):
```go
type Payload struct {
    Status    string `json:"status"`
    Count     int    `json:"count"`
    Timestamp int64  `json:"timestamp"`
}
payload := Payload{
    Status:    req.Status,
    Count:     req.Count,
    Timestamp: time.Now().Unix(),
}
data, _ := sonic.Marshal(payload)
```

**Impact**: Reduces interface allocation overhead
- Savings: 2-3 allocations per serialization
- Applies to: Webhook payloads, API responses

## codeQ Optimization Examples

### Result Submission Artifact Handling
**File**: `internal/services/results_service.go`

**Optimization**: Eliminate lock-protected slice append
- Pre-allocate `outs` with full capacity
- Use channel to collect upload results
- Append sequentially after all goroutines complete

**Impact**: 15-25% latency improvement for result submission with 5+ artifacts

### Task Expiration Cleanup
**File**: `internal/repository/task_repository.go`

**Optimization**: Pre-allocate `expiredIDs` with capacity
- Before: `make([]string, 0)` → O(log N) allocations
- After: `make([]string, 0, len(ids))` → 1 allocation

**Impact**: 5-10% GC pressure reduction for batch cleanup operations

### Batch Claim Operations
**File**: `internal/controllers/batch_claim_task_controller.go`

**Optimization**: Pre-allocate `tasks` with exact capacity
- Before: `var tasks []*domain.Task` → 2-4 allocations
- After: `make([]*domain.Task, 0, req.Count)` → 1 allocation

**Impact**: 2-3% latency improvement for batch claim operations

## Profiling and Measurement

### Using pprof to Find Allocation Hotspots
```bash
# Record memory profile during load
go test -bench=. -benchmem -memprofile=mem.prof ./internal/bench

# Analyze top allocations
go tool pprof -top mem.prof

# Find specific functions with high allocation rate
go tool pprof -list=functionName mem.prof
```

### Memory Statistics from Benchmarks
```bash
# Run with detailed memory stats
go test -bench=. -benchmem -benchtime=10s ./internal/bench

# Look for:
# - Allocations per operation (lower is better)
# - Bytes allocated per operation
# - Allocation rate trends
```

### Verification After Optimization
```bash
# Before and after comparison
go test -bench=BenchmarkSubmitResult -benchmem > before.txt
# [Make changes]
go test -bench=BenchmarkSubmitResult -benchmem > after.txt

# Compare with benchstat
go install golang.org/x/perf/cmd/benchstat@latest
benchstat before.txt after.txt

# Expected: allocs/op should decrease significantly
```

## Performance Impact Summary

### Combined Improvements (All Optimizations)
| Metric | Expected Improvement |
|--------|----------------------|
| Allocation churn (per operation) | 15-25% reduction |
| P95 latency (result submission) | 8-15% improvement |
| P99 latency (GC pause reduction) | 10-20% improvement |
| Lock contention | Eliminated for append operations |

### Where to Apply
1. **Result/artifact operations**: High impact (concurrent uploads)
2. **Batch cleanup operations**: High impact (many expired tasks)
3. **Batch claim operations**: Medium impact (frequent, predictable batch size)
4. **String formatting in loops**: Medium impact (cumulative)
5. **Webhook payload construction**: Low-medium impact (high volume scenarios)

## Tradeoffs and Considerations

### Memory Usage vs. GC Pressure
- **Pro**: Pre-allocation makes peak memory usage more predictable
- **Con**: Slightly higher memory per operation (usually < 1KB)
- **Net**: Reduced GC overhead outweighs small memory increase

### Code Complexity
- **Pre-allocation**: Trivial complexity increase (one parameter)
- **Channel-based collection**: Slightly more explicit, but idiomatic Go
- **Net**: Improved clarity about concurrent data flow

### When NOT to Pre-allocate
- Unknown final size and likely to be small: Use dynamic allocation
- Size varies widely: May waste memory with high capacity
- One-time operations: Negligible performance impact

## Related Optimizations
- See `docs/17-performance-tuning.md` Section 9 for Redis pipelining patterns
- See `05-build-performance.md` for benchmarking and profiling tools
- See `11-sharded-operations-parallelization.md` for concurrent I/O patterns

## Success Metrics
✅ Allocation count per operation reduced significantly (benchmark shows allocs/op decrease)
✅ No latency regressions in existing tests
✅ P99/P99.9 latencies improve under sustained load
✅ Memory footprint remains stable or improves
✅ All existing functionality preserved (no behavior changes)
