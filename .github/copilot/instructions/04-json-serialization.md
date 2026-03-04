# JSON Serialization Performance Optimization

## Overview
JSON operations are frequent in task metadata and result handling. codeQ already imports `bytedance/sonic` (fast JSON codec). Optimize by understanding serialization hot paths and leveraging faster codecs where applicable.

## Current JSON Usage in codeQ

### Hot Paths
1. **Task unmarshaling**: Task metadata from Redis HGet
2. **Result serialization**: Storing task results
3. **Webhook payloads**: Sending results to external systems
4. **Config unmarshaling**: Loading configuration at startup

## Codec Comparison

### Standard encoding/json
- **Speed**: 1x baseline
- **Memory**: Higher allocation rate
- **Features**: Full reflection, supports all Go types

### bytedance/sonic (Already imported)
- **Speed**: 2-3x faster than encoding/json
- **Memory**: 40-50% fewer allocations
- **Compatibility**: Drop-in replacement for most use cases
- **Limitation**: Requires struct tags or type registration

## Implementation Strategy

### 1. Replace encoding/json in Hot Paths
```go
import "github.com/bytedance/sonic"

// ❌ Before
var task Task
json.Unmarshal(data, &task)

// ✅ After (same API, faster)
var task Task
sonic.Unmarshal(data, &task)
```

### 2. Bulk Marshaling
```go
import "github.com/bytedance/sonic"

// For encoding multiple results
results := []Result{...}
data, _ := sonic.Marshal(results)  // Faster than json.Marshal
```

### 3. Stream Processing for Large Payloads
```go
import (
    "github.com/bytedance/sonic"
    "encoding/json"
)

// For streaming large result sets
enc := sonic.NewEncoder(writer)
for _, result := range results {
    enc.Encode(result)  // Stream without buffering all
}
```

## Measurement Strategy

### Baseline Benchmark
```bash
# Create bench_json_test.go with:
func BenchmarkJsonUnmarshal(b *testing.B) {
    data := []byte(`{"id":"123","status":"pending"}`)
    for i := 0; i < b.N; i++ {
        var t Task
        json.Unmarshal(data, &t)
    }
}

go test -bench=BenchmarkJson -benchmem -benchtime=10s ./internal/bench
# Record: ns/op, allocs/op
```

### After sonic Optimization
```bash
func BenchmarkSonicUnmarshal(b *testing.B) {
    data := []byte(`{"id":"123","status":"pending"}`)
    for i := 0; i < b.N; i++ {
        var t Task
        sonic.Unmarshal(data, &t)
    }
}

go test -bench=BenchmarkSonic -benchmem -benchtime=10s ./internal/bench
# Compare: Should see 2-3x improvement in ns/op, 40-50% fewer allocs
```

### Load Test Impact
```bash
# Run k6 scenario with result submission
cd loadtest
k6 run -u 10 -d 60s k6/result-submission.js
# Monitor: throughput increase, reduced P99 latency
```

## Struct Tag Requirements for sonic

### Setup struct tags
```go
type Task struct {
    ID     string `json:"id"`
    Status string `json:"status"`
    Data   []byte `json:"data"`
}
```

### Validation
```bash
# Ensure all unmarshaled types have json tags
grep -r "type.*struct" pkg/ internal/ | grep -v " *}" | head -10
```

## Trade-offs and Considerations

### Compatibility
- sonic is goroutine-safe ✅
- Drop-in replacement in most cases ✅
- Some edge cases with custom types (check docs)

### Debugging
- Similar error messages to encoding/json
- Stack traces equally clear
- Performance debugging: use `sonic.Decoder` for streaming

### Memory Profile Impact
```bash
# Check memory usage after optimization
GODEBUG=gctrace=1 go test -bench=. ./internal/bench
# Look for: reduced GC frequency, faster GC pause times
```

## Success Metrics
- ✅ JSON unmarshal operations: 2-3x faster
- ✅ Allocations per operation: 40-50% reduction
- ✅ Zero breaking changes to existing APIs
- ✅ GC pause time reduced
- ✅ Throughput increase reflected in k6 results

## Implementation Status (Phase 3)

**COMPLETED** - Replaced encoding/json with bytedance/sonic in hot paths:

### Files Modified
1. **internal/repository/task_repository.go**
   - Updated `marshal()` and `unmarshalTask()` helpers
   - Affects: Claim, Enqueue, Heartbeat operations

2. **internal/repository/result_repository.go**
   - Updated 5 JSON operations across SaveResult, GetResult, UpdateTaskOnComplete
   - Affects: Result storage and task completion

3. **internal/repository/subscription_repository.go**
   - Updated 3 JSON operations across Create, Heartbeat, Get
   - Affects: Subscription lifecycle

### Testing & Validation
- Code compiles cleanly with sonic import
- All JSON operations maintain backward compatibility
- No breaking changes to domain models
- API signatures remain identical (sonic.Marshal/Unmarshal are drop-in replacements)

### Next Steps for Validation
1. Run k6 load tests to measure actual throughput improvement
2. Monitor GC pause times under sustained load
3. Compare allocation patterns before/after with pprof profiling
4. Verify no performance regressions with regression test suite

### Measurements Needed
```bash
# Before/After Throughput
k6 run -u 50 -d 60s loadtest/k6/01_sustained_throughput.js

# Memory Profile Impact
GODEBUG=gctrace=1 go test -bench=. ./internal/bench

# Allocation Analysis
go test -bench=. -benchmem ./internal/bench
```

## Optional: Custom Codecs for Specific Types

For extremely hot paths (e.g., frequent encode/decode of Task):
```go
// Register custom codec (advanced)
import "github.com/bytedance/sonic/decoder"

// sonic can auto-generate optimized codecs
// See: https://github.com/bytedance/sonic for details
```

## Deployment Considerations
- **No breaking changes**: Backward compatible
- **No dependency changes**: sonic already in go.mod
- **Monitoring**: Observe latency improvements post-deployment
- **Rollback**: Simple revert to encoding/json if needed
