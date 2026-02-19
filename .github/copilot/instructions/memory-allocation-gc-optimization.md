# Memory Allocation and GC Optimization Guide

## GC Pressure in High-Throughput Systems

Excessive allocations trigger frequent garbage collection, causing latency spikes. This guide covers identifying and eliminating hot-path allocations.

## Quick Allocation Analysis

### 1. Benchmark Allocation Count

```bash
# Run benchmark with memory stats
go test -bench=. -benchmem ./internal/bench

# Look for "B/op" (bytes per operation) and "Allocs/op"
# Example output:
# BenchmarkScheduler_CreateClaimComplete-4  15284  78256 ns/op  4592 B/op  123 allocs/op
```

**Interpretation**:
- 4592 B/op = 4.5KB allocated per claim
- 123 allocs/op = 123 separate allocations
- This means frequent small allocations, GC pressure

### 2. Memory Profile Analysis

```bash
# Generate memory profile
go test -memprofile=mem.prof \
  -bench=BenchmarkScheduler_CreateClaimComplete \
  -benchtime=10s ./internal/bench

# Show allocation count (number of allocations)
go tool pprof -alloc_objects mem.prof | top -cum | head -20

# Show allocation space (bytes used)
go tool pprof -alloc_space mem.prof | top -cum | head -20
```

**Focus on functions allocating > 5% of total allocation count**.

### 3. Identify Repeated Allocations

```bash
# List source code with allocation counts
go tool pprof mem.prof
(pprof) list FunctionName

# Look for loops allocating in each iteration
```

## Common Allocation Patterns to Fix

### Pattern 1: JSON Struct Allocation

**Before** (allocates new struct every time):
```go
func handleTask(raw []byte) {
  var task TaskDTO // Allocated on each call
  json.Unmarshal(raw, &task)
  // ...
}
```

**After** (reuse struct):
```go
var taskPool = &sync.Pool{
  New: func() interface{} { return &TaskDTO{} },
}

func handleTask(raw []byte) {
  task := taskPool.Get().(*TaskDTO)
  defer taskPool.Put(task)
  
  json.Unmarshal(raw, task)
  // ...
}
```

### Pattern 2: Temporary Buffer Allocation

**Before** (allocates per operation):
```go
func encodeTask(t *Task) []byte {
  buf := bytes.NewBuffer(nil) // New buffer
  json.NewEncoder(buf).Encode(t)
  return buf.Bytes()
}
```

**After** (reuse buffer):
```go
var encoderPool = &sync.Pool{
  New: func() interface{} { return &bytes.Buffer{} },
}

func encodeTask(t *Task) []byte {
  buf := encoderPool.Get().(*bytes.Buffer)
  defer encoderPool.Put(buf)
  buf.Reset()
  
  json.NewEncoder(buf).Encode(t)
  return buf.Bytes()
}
```

### Pattern 3: Map/Slice Allocation

**Before** (allocates temporary collections):
```go
func filterTasks(tasks []Task) []Task {
  result := make([]Task, 0) // Initial capacity unknown
  for _, t := range tasks {
    if isValid(t) {
      result = append(result, t) // May reallocate
    }
  }
  return result
}
```

**After** (pre-allocate):
```go
func filterTasks(tasks []Task, result []Task) int {
  count := 0
  for _, t := range tasks {
    if isValid(t) && count < cap(result) {
      result[count] = t
      count++
    }
  }
  return count
}
```

## Measurement Protocol

### Baseline

```bash
go test -bench=. -benchmem ./internal/bench > baseline.txt
grep "Allocs/op\|B/op" baseline.txt
```

### After Optimization

```bash
go test -bench=. -benchmem ./internal/bench > optimized.txt
grep "Allocs/op\|B/op" optimized.txt
```

### Calculate Impact

```bash
# Example:
# Before: 4592 B/op, 123 allocs/op
# After:  2048 B/op, 45 allocs/op
# Improvement: 55% less memory, 63% fewer allocations
```

## GC Impact Verification

Run load test and monitor GC pauses:

```bash
# Start services with GC logging
GODEBUG=gctrace=1 docker compose up codeq

# Run load test
DURATION=5m docker compose --profile loadtest run --rm k6 run /scripts/01_sustained_throughput.js

# Look for gc output:
# gc 1243 @1.234s 2%: 0.045+1.2+0.030 ms clock, 0.091+0.30/0.60/1.5+0.060 ms cpu
# Lower milliseconds = less GC impact
```

## Profiling GC Pressure

```go
// In test or benchmark
import "runtime"

var m1, m2 runtime.MemStats
runtime.ReadMemStats(&m1)

// Run operation
for i := 0; i < 1000; i++ {
  doOperation()
}

runtime.ReadMemStats(&m2)

allocs := m2.Mallocs - m1.Mallocs
fmt.Printf("Allocations: %d\n", allocs)
fmt.Printf("GC runs: %d\n", m2.NumGC - m1.NumGC)
```

## Optimization Priority

1. **Hot path allocations** (claim, result submission): High impact
2. **Repeated allocations in loops**: Medium impact
3. **Batch operation allocations**: Medium impact
4. **Non-critical path allocations**: Low priority

## Expected Improvements

- Reducing allocs/op by 50% → ~20% latency improvement
- Reducing B/op by 50% → ~10-15% GC pause reduction
- Combined → Lower P99, steadier throughput

## Tools and Commands

```bash
# Profile allocations
go test -memprofile=mem.prof -bench=. ./internal/bench
go tool pprof -alloc_objects mem.prof

# Compare profiles
go tool pprof -base=baseline.prof optimized.prof

# Live GC stats
curl http://localhost:6060/debug/pprof/heap
```
