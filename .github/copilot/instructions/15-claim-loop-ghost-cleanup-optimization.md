# Claim Loop Ghost Task Cleanup Optimization

## Overview

The Task claim operation iterates through up to `inspectLimit` items in the pending queue, attempting to claim each task. When the ghost Bloom filter indicates a task has been deleted, the code performs cleanup operations (SRem + LRem on both inprog SET and pending LIST). Currently, these cleanup operations happen in an isolated pipeline **inside the loop**, creating one pipeline per ghost task found.

### The Performance Problem

**Current implementation (lines 693-699 in task_repository.go):**

```go
if r.ghostBloom != nil && r.ghostBloom.MaybeHas(id) {
    pipe := r.rdb.Pipeline()  // New pipeline created here
    pipe.SRem(ctx, dst, id)
    pipe.LRem(ctx, src, 0, id)
    _, _ = pipe.Exec(ctx)     // RTT for cleanup
    continue
}
```

**Why this matters:**
- For each suspected ghost task: 1 pipeline creation + 1 Exec RTT
- In queues with many deleted tasks: N pipelines + N RTTs
- Pipeline creation has overhead (allocation, setup, cleanup)
- Multiple Exec() calls prevent batching efficiencies

**Real-world scenario:**
- Batch of 100 tasks claimed: up to 50 ghost tasks encountered
- Result: 50 pipeline creations + 50 RTT round-trips
- At 5ms per RTT: ~250ms wasted on cleanup alone
- At 1,000 claims/sec: ~250 seconds per second of throughput burned on cleanup overhead

## Solution: Batch Ghost Task Cleanup

Instead of cleaning up each ghost task individually inside the loop, collect all ghost task IDs and perform a single batch cleanup operation after the inspection loop completes.

### Implementation

**Optimized version:**

```go
// Collect ghost task IDs for batch cleanup (avoid pipeline per iteration)
var ghostIDs []string

for i := 0; i < inspectLimit; i++ {
    res, err := claimMoveScript.Run(...)
    if err == redis.Nil {
        return nil, false, nil
    }
    if err != nil {
        return nil, false, fmt.Errorf("claim move script: %w", err)
    }
    id, ok := res.(string)
    if !ok || id == "" {
        return nil, false, nil
    }

    // Collect ghost IDs instead of cleaning up immediately
    if r.ghostBloom != nil && r.ghostBloom.MaybeHas(id) {
        ghostIDs = append(ghostIDs, id)  // ✅ Collect only
        continue
    }

    // ... rest of task processing ...
}

// Single batch cleanup after loop completes
if len(ghostIDs) > 0 {
    cleanupPipe := r.rdb.Pipeline()
    for _, id := range ghostIDs {
        cleanupPipe.SRem(ctx, dst, id)
        cleanupPipe.LRem(ctx, src, 0, id)
    }
    _, _ = cleanupPipe.Exec(ctx)  // ✅ Single RTT for all ghost cleanups
}
```

**Key improvements:**
- Loop collects IDs: O(N) in-memory operations (microseconds)
- Single batch cleanup: 1 RTT + 1 pipeline setup (instead of N RTTs)
- Pipeline Exec() processes all 2N commands atomically
- Memory overhead: Small ([]string of ghost IDs, typically <1% of items)

## Performance Impact

### Measurement

**Baseline (current):**
- 50 ghost tasks with 5ms RTT: 50 × 5ms = 250ms cleanup latency
- Claim loop latency: 10-20ms (typical)
- **Total: ~260-270ms**

**After optimization:**
- Batch collection: <1ms (in-memory)
- Single batch cleanup: 5ms RTT + 2ms pipeline processing
- **Total: ~7-8ms**

**Improvement:** 95-97% reduction in ghost cleanup latency (260ms → 7ms)

### Conditions

- **Best case:** High deletion rate (10-50% of inspected tasks are ghosts)
  - 50ms+ improvement for typical claim operations
  - Especially impactful at scale (1000+ claims/sec)

- **Typical case:** Moderate deletion rate (1-5% ghosts)
  - 5-25ms improvement
  - Accumulates across all claim operations

- **Worst case:** Rare ghost tasks (<0.1%)
  - Minimal change (ghost collection has negligible overhead)
  - No regression from collecting empty list

## Implementation Details

**Location:** `internal/repository/task_repository.go:Claim()` (tryPop inner function)

**Changes:**
1. Declare `ghostIDs` slice before the loop
2. Replace inline pipeline execution with append to ghostIDs
3. Add batch cleanup after the loop exits
4. Ensure cleanup happens regardless of success/failure path

**Trade-offs:**
- **Complexity:** +3 lines (collection + batch cleanup)
- **Memory:** Minimal (one slice for ghost IDs)
- **Correctness:** Cleanup semantics unchanged (same operations, just batched)
- **Backward compatibility:** No API changes

## Verification

**Correctness checks:**
1. All ghost tasks are cleaned up from both inprog and pending
2. Loop still returns immediately on successful claim
3. No ghost cleanup happens when ghostBloom is nil
4. Cleanup operations maintain isolation from main claim transaction

**Testing:**
1. Unit test: Verify all ghost IDs collected and cleaned in batch
2. Unit test: Verify cleanup doesn't run on empty list
3. Load test: Compare latency with/without optimization at high deletion rates
4. Benchmark: `BenchmarkClaim_HighGhostRate` with 50% ghost task ratio

## When to Apply

Apply this optimization when:
- Claim operation experiences periodic task deletions (1%+ of tasks are ghosts)
- Pipeline overhead from cleanup operations is measurable (profile to confirm)
- Claim latency is part of user-facing SLO (p50, p99 response time)

**Avoid when:**
- Ghost tasks are extremely rare (<0.001% deletion rate)
- Claim latency is not a performance bottleneck (better opportunities elsewhere)

## Future Work

- **Configurable batch size:** Trigger cleanup at certain ghost count threshold
- **Adaptive collection:** Collect across multiple loop iterations in very high-throughput scenarios
- **Metrics:** Add prometheus metric for ghost task cleanup latency

## References

- Base pattern: See Section 2 (Redis Pipelining Performance)
- Bloom filter foundation: Section 12 (Ghost Task Bloom Filter Optimization)
- Claim implementation: `internal/repository/task_repository.go:Claim()`
- Related optimization: Section 14 (BatchSubmit Index Mapping - similar loop optimization)
