# Concurrent I/O Optimization with Bounded Goroutines

## Overview

External I/O operations (artifact uploads, webhook dispatches, HTTP callbacks) are common bottlenecks in task execution systems. This guide documents techniques to parallelize blocking I/O operations while maintaining resource control.

## Problem

Sequential I/O operations create artificial latency multiplicities:

- **10 artifact uploads @ 200ms each**: 2000ms total (sequential) vs 400ms (with 5 concurrent workers)
- **50 webhook dispatches @ 500ms RTT**: 25 seconds total (sequential) vs ~2.5 seconds (with 20 concurrent workers)
- **Resource exhaustion risk**: Unlimited goroutines can exhaust memory and file descriptors

### Real-World Example: Artifact Uploads

Original code in `internal/services/results_service.go`:

```go
// Sequential: N uploads = N × upload_latency
for _, a := range req.Artifacts {
    data, err := s.repo.DecodeBase64(a.ContentBase64)
    // ... error handling ...
    url, err := s.uploader.UploadBytes(taskCtx, objPath, a.ContentType, data)
    outs = append(outs, domain.ArtifactOut{Name: a.Name, URL: url})
}
```

## Solution: Bounded Semaphore Pattern

Use a buffered channel as a semaphore to limit concurrent goroutines while parallelizing I/O.

### Implementation Pattern

```go
var outs []domain.ArtifactOut
var outsMu sync.Mutex

// Collect work items
var toUpload []domain.SubmitArtifact
for _, a := range req.Artifacts {
    toUpload = append(toUpload, a)
}

// Concurrent execution with bounded concurrency
if len(toUpload) > 0 {
    sem := make(chan struct{}, 5)  // Max 5 concurrent uploads
    var wg sync.WaitGroup
    var uploadErr error
    var errMu sync.Mutex

    for _, a := range toUpload {
        wg.Add(1)
        go func(artifact domain.SubmitArtifact) {
            defer wg.Done()
            sem <- struct{}{}        // Acquire semaphore slot
            defer func() { <-sem }() // Release slot

            // Fail fast on first error
            if uploadErr != nil {
                return
            }

            // Perform I/O operation
            url, err := s.uploader.UploadBytes(ctx, path, contentType, data)
            
            // Capture first error only
            errMu.Lock()
            if uploadErr == nil && err != nil {
                uploadErr = err
            }
            errMu.Unlock()

            // Accumulate results with mutex protection
            outsMu.Lock()
            outs = append(outs, domain.ArtifactOut{Name: artifact.Name, URL: url})
            outsMu.Unlock()
        }(a)
    }

    wg.Wait()
    if uploadErr != nil {
        return nil, uploadErr
    }
}
```

### Key Patterns

1. **Semaphore via buffered channel**: `make(chan struct{}, N)` limits concurrent goroutines
2. **Fail-fast**: First error stops accepting new work; ongoing goroutines complete
3. **Mutex protection**: Results and errors accessed under lock
4. **Context passing**: Each goroutine receives required data as function parameter to avoid closure bugs

## Performance Impact

### Artifact Upload Optimization (ResultsService)

- **Scenario**: 10 artifacts, 200ms per upload
- **Before**: 2000ms (sequential, 10 × 200ms)
- **After**: 400ms (concurrent with semaphore of 5, ⌈10/5⌉ × 200ms)
- **Improvement**: 80% latency reduction

### Webhook Dispatch Optimization (NotifierService)

- **Scenario**: 50 subscriptions with 500ms avg response time
- **Before**: 25 seconds (sequential)
- **After**: ~2.5 seconds (concurrent with semaphore of 20)
- **Improvement**: 90% latency reduction

## Configuration

Semaphore size depends on resource constraints:

- **Artifact uploads**: `5-10` (limited by file descriptor availability and memory)
- **HTTP webhooks**: `10-20` (DNS/connection pool limits)
- **Database operations**: `5-15` (connection pool size)

## When to Apply

Use bounded semaphore concurrency when:

1. Operations are I/O-bound (network, storage, database)
2. Latency matters (user-facing requests, critical paths)
3. Resource exhaustion is a concern (many concurrent requests)
4. Order of execution doesn't matter
5. Partial results are acceptable (fail-fast on first error)

## When NOT to Apply

Avoid this pattern when:

- Operations must be sequential (ordering required)
- Single I/O is already fast (< 5ms latency)
- Resource constraints are very tight (embedded systems)
- Error handling requires processing all operations before deciding on failure

## Measurement Strategy

Benchmark concurrent vs. sequential implementations:

```bash
# Measure submission latency with varying artifact counts
go test -bench=BenchmarkSubmitArtifacts -benchmem -benchtime=10s ./internal/services
```

Expected results:
- Sequential: linear growth (1 artifact = 200ms, 10 artifacts = 2000ms)
- Concurrent (semaphore 5): sublinear growth (1-5 artifacts ≈ 200ms, 10 artifacts ≈ 400ms)

## Related Optimizations

- **Redis pipelining** (guide: 02-redis-pipelining.md): Similar pattern for Redis commands
- **Batch operations** (guide: 06-redis-batch-optimization.md): Pre-collect work before concurrent execution
- **Worker claim parallelization** (guide: 10-sharded-operations-parallelization.md): Concurrent Redis sharded operations

