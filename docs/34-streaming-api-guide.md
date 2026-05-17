# gRPC Streaming API Guide

> **Status**: Production-ready (Phase 3+). Streaming APIs provide 2-3× throughput vs REST by amortizing authentication and enabling async pipelining. Use when latency- or throughput-sensitive.

## Overview

codeQ provides high-performance gRPC streaming APIs as alternatives to the REST API. The streaming protocols enable:

- **Producer Streaming**: Submit tasks in a long-lived bidirectional stream with async pipelining (~33k tasks/sec per stream)
- **Worker Streaming**: Claim and complete tasks in parallel slots with configurable concurrency

Both APIs follow the same semantic guarantees as their REST equivalents but achieve 2-3× higher throughput by:

1. **Single auth overhead**: Auth happens once at stream-open, not per-request
2. **Async pipelining**: Send multiple requests before receiving replies
3. **Batch messages**: Coalesce multiple operations into single gRPC frames
4. **Reduced Round-Trip Time (RTT)**: One persistent connection vs per-call TCP handshakes

---

## Table of Contents

1. [Tutorial: Producer Streaming](#tutorial-producer-streaming)
2. [Tutorial: Worker Streaming](#tutorial-worker-streaming)
3. [How-To: Enable TLS](#how-to-enable-tls)
4. [How-To: Handle Errors](#how-to-handle-errors)
5. [How-To: Monitor Streams](#how-to-monitor-streams)
6. [Technical Reference](#technical-reference)
7. [Performance Explanation](#performance-explanation)

---

## Tutorial: Producer Streaming

### Goal
Submit tasks via a persistent gRPC stream instead of individual HTTP POST requests.

### Prerequisites

1. A running codeQ server with gRPC producer streaming enabled (default port: `9092`)
2. A valid bearer token with `codeq:create` scope
3. The Go producer client SDK: `pkg/producerclient`

### Step 1: Create a Producer Client

```go
import "github.com/osvaldoandrade/codeq/pkg/producerclient"

client, err := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092",
    Token: "your-bearer-token",
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
```

**Configuration fields:**

- `Addr`: gRPC server address (required). Format: `host:port`
- `Token`: Bearer token (required). Must have `codeq:create` scope
- `DialOptions`: gRPC dial options for TLS/mTLS (optional). Defaults to insecure
- `Logger`: Structured logger (optional). Defaults to `slog.Default()`

### Step 2: Open a Session

```go
ctx := context.Background()
session, err := client.Connect(ctx)
if err != nil {
    log.Fatal(err)
}
defer session.Close()

// After Connect succeeds, the session is authenticated and ready
fmt.Printf("TenantID: %s, Subject: %s\n", session.TenantID(), session.Subject())
```

**What happens:**

1. Client opens bidirectional gRPC stream
2. Sends `Hello` message with token
3. Server validates token, resolves tenant and subject
4. Server responds with `HelloAck`
5. Reader goroutine spawns to handle async acks

### Step 3: Submit Tasks with Pipelining

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

// Submit multiple tasks from different goroutines concurrently
// Each Produce blocks only until the matching ack arrives
taskID1, err := session.Produce(ctx, producerclient.CreateRequest{
    Command: "GENERATE_MASTER",
    Payload: []byte(`{"format":"mp4"}`),
    Priority: 1,
})
if err != nil {
    log.Printf("Create failed: %v", err)
}
fmt.Printf("Created task: %s\n", taskID1)

// While first task is in flight, send another (pipelining)
taskID2, err := session.Produce(ctx, producerclient.CreateRequest{
    Command: "RENDER_VIDEO",
    Payload: []byte(`{"codec":"h264"}`),
    MaxAttempts: 3,
})
```

**Key points:**

- `Produce()` is safe to call concurrently from many goroutines
- Each call blocks until the matching `CreateAck` arrives (not until all prior sends complete)
- Multiple in-flight Produces achieve pipeline parallelism
- Timeout applies to the individual Produce, not the entire batch

### Step 4: Batch Submission (Optional)

For even higher throughput, batch multiple tasks into one message:

```go
results, err := session.ProduceBatch(ctx, []producerclient.CreateRequest{
    {Command: "TASK_A", Payload: []byte(`{"id":1}`)},
    {Command: "TASK_B", Payload: []byte(`{"id":2}`)},
    {Command: "TASK_C", Payload: []byte(`{"id":3}`)},
})
if err != nil {
    log.Fatal(err)
}

for i, res := range results {
    if res.Err != nil {
        log.Printf("Task %d failed: %v", i, res.Err)
    } else {
        log.Printf("Task %d created: %s", i, res.TaskID)
    }
}
```

**Benefits:**

- One gRPC Send + one ServerEvent vs N Sends/Acks
- Server processes all tasks in parallel internally
- Amortizes gRPC framing overhead

---

## Tutorial: Worker Streaming

### Goal
Claim and complete tasks via a persistent gRPC stream with configurable concurrency.

### Prerequisites

1. A running codeQ server with gRPC worker streaming enabled (default port: `9091`)
2. A valid bearer token with `codeq:claim` and `codeq:result` scopes
3. The Go worker client SDK: `pkg/workerclient`

### Step 1: Create a Worker Client

```go
import "github.com/osvaldoandrade/codeq/pkg/workerclient"

client, err := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",
    Token:       "your-bearer-token",
    WorkerID:    "worker-1",
    Concurrency: 4,        // Run 4 task slots in parallel
    BatchSize:   1,        // Claim 1 task per Ready (or >1 for batching)
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
```

**Configuration fields:**

- `Addr`: gRPC server address (required)
- `Token`: Bearer token (required). Must have `codeq:claim` and `codeq:result` scopes
- `WorkerID`: Worker identifier for lease ownership (optional). Defaults to JWT subject
- `Commands`: Restrict task types this worker pulls (optional)
- `Concurrency`: Number of parallel task slots (default: 1)
- `BatchSize`: Tasks to claim per Ready (0 or 1 = single task, >1 = batch)
- `LeaseSeconds`: Lease duration (optional). 0 = server default (usually 300s)
- `IdleBackoff`: Wait time before re-sending Ready when queue is empty (default: 50ms)

### Step 2: Implement a Task Handler

```go
func handleTask(ctx context.Context, task workerclient.Task) workerclient.Result {
    log.Printf("Got task %s: %s", task.ID, task.Command)
    
    switch task.Command {
    case "GENERATE_MASTER":
        // Process the task
        output, err := generateMaster(ctx, task.Payload)
        if err != nil {
            // Return failure; will retry unless MaxAttempts exceeded
            return workerclient.Failed(task.ID, fmt.Sprintf("generation failed: %v", err))
        }
        return workerclient.Completed(task.ID, output)
        
    case "RENDER_VIDEO":
        // Another task type
        output, err := renderVideo(ctx, task.Payload)
        if err != nil {
            // Return nack to requeue after delay
            return workerclient.Nack(task.ID, 30) // 30-second delay
        }
        return workerclient.Completed(task.ID, output)
        
    default:
        return workerclient.Failed(task.ID, "unknown command")
    }
}
```

**Result types:**

- `Completed(taskID, output)`: Task succeeded; `output` is JSON payload
- `Failed(taskID, error)`: Task failed permanently; respects `MaxAttempts`
- `Nack(taskID, delaySeconds)`: Requeue after delay; task becomes claimable again
- `Abandon(taskID)`: Release lease without marking failed; useful for graceful shutdown

### Step 3: Run the Worker

```go
ctx := context.Background()
err := client.Run(ctx, handleTask)
if err != nil {
    log.Printf("Worker stream closed: %v", err)
}
```

**What happens:**

1. Opens bidirectional gRPC stream
2. Sends `Hello` with token; receives `HelloAck`
3. Spawns N concurrent slots (N = `Concurrency`)
4. Each slot independently:
   - Sends `Ready` (with `count` = `BatchSize` if batching)
   - Receives `TaskAssignment` or `TaskBatch`
   - Calls `handleTask()` for each task
   - Sends `Result` / `ResultBatch`
   - Loops back to Ready
5. Continues until `ctx` is cancelled or stream error occurs

### Step 4: Graceful Shutdown

```go
// Create a cancellable context
ctx, cancel := context.WithCancel(context.Background())

// Run worker in a goroutine
go func() {
    if err := client.Run(ctx, handleTask); err != nil {
        log.Printf("Worker error: %v", err)
    }
}()

// When you want to shut down (e.g., SIGTERM):
cancel()

// Give in-flight tasks 30 seconds to complete
time.Sleep(30 * time.Second)
client.Close()
```

**Behavior:**

- Cancelling `ctx` stops accepting new Ready messages
- In-flight tasks complete normally
- Existing leases expire or are released via `Abandon`

---

## How-To: Enable TLS

### Producer Client with TLS

```go
import (
    "crypto/tls"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
)

tlsConfig := &tls.Config{
    ServerName: "codeq.example.com",
    // For self-signed certs in testing:
    // InsecureSkipVerify: true,
}

client, err := producerclient.New(producerclient.Config{
    Addr:  "codeq.example.com:9092",
    Token: "bearer-token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    },
})
```

### Worker Client with mTLS

```go
import (
    "crypto/tls"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
)

// Load client cert and key
cert, err := tls.LoadX509KeyPair("client-cert.pem", "client-key.pem")
if err != nil {
    log.Fatal(err)
}

tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{cert},
    ServerName:   "codeq.example.com",
}

client, err := workerclient.New(workerclient.Config{
    Addr:     "codeq.example.com:9091",
    Token:    "bearer-token",
    WorkerID: "worker-1",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    },
})
```

---

## How-To: Handle Errors

### Producer Errors

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

taskID, err := session.Produce(ctx, producerclient.CreateRequest{
    Command: "GENERATE_MASTER",
    Payload: []byte(`{"format":"mp4"}`),
})

switch {
case errors.Is(err, context.DeadlineExceeded):
    log.Println("Request timeout")
case errors.Is(err, context.Canceled):
    log.Println("Context cancelled")
default:
    if err != nil {
        log.Printf("Create failed: %v", err)
        // Err may be from server validation or stream I/O
    }
}
```

**Common server errors:**

- `invalid command`: Task command type not recognized
- `payload too large`: Payload exceeds maximum size (~1MB typical)
- `webhook invalid`: Webhook URL failed validation
- `tenant not found`: Bearer token not associated with valid tenant

### Worker Errors

```go
func handleTask(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Long-running work
    select {
    case <-ctx.Done():
        // Graceful shutdown or timeout
        return workerclient.Abandon(task.ID)
    default:
    }
    
    output, err := doWork(ctx, task)
    if err != nil {
        if isTempError(err) {
            // Temporary issue; requeue after delay
            return workerclient.Nack(task.ID, 30)
        }
        // Permanent failure
        return workerclient.Failed(task.ID, err.Error())
    }
    return workerclient.Completed(task.ID, output)
}
```

### Stream-Level Errors

If the gRPC stream encounters a fatal error (e.g., auth failure, network loss), `Run()` or `Connect()` returns an error. Common causes:

- **`Unauthenticated`**: Bearer token invalid or expired
- **`PermissionDenied`**: Token lacks required scopes
- **`Unavailable`**: Server unreachable or shutting down
- **`Internal`**: Server error; check server logs

**Retry strategy:**

```go
for attempt := 0; attempt < 3; attempt++ {
    if err := client.Run(ctx, handleTask); err != nil {
        log.Printf("Attempt %d failed: %v", attempt+1, err)
        select {
        case <-ctx.Done():
            return
        case <-time.After(time.Second * time.Duration(math.Pow(2, float64(attempt)))):
            continue
        }
    }
    break
}
```

---

## How-To: Monitor Streams

### Producer Metrics

```go
// Use session.TenantID() and session.Subject() for logging/metrics
sess, err := client.Connect(ctx)
if err != nil {
    log.Fatal(err)
}

log.Printf("Connected to tenant %s as subject %s", sess.TenantID(), sess.Subject())

// Each Produce call is one metric point; you can track:
// - Count of successful creates
// - Histogram of latencies (from send to ack)
// - Count of errors
// - Batch vs single rates

start := time.Now()
taskID, err := sess.Produce(ctx, req)
duration := time.Since(start)

if err != nil {
    metrics.ProducerError.Inc()
} else {
    metrics.ProducerSuccess.Inc()
    metrics.ProducerLatency.Observe(duration.Seconds())
}
```

### Worker Metrics

```go
func handleTask(ctx context.Context, task workerclient.Task) workerclient.Result {
    start := time.Now()
    defer func() {
        duration := time.Since(start)
        metrics.TaskLatency.Observe(duration.Seconds())
    }()
    
    // Log task metadata for distributed tracing
    log.Printf("task_id=%s command=%s priority=%d", 
        task.ID, task.Command, task.Priority)
    
    result := doWork(ctx, task)
    
    // Track result type
    switch r := result.(type) {
    case workerclient.CompletedResult:
        metrics.TaskCompleted.Inc()
    case workerclient.FailedResult:
        metrics.TaskFailed.Inc()
    case workerclient.NackResult:
        metrics.TaskNacked.Inc()
    }
    
    return result
}
```

### Connection Health

```go
import "google.golang.org/grpc/connectivity"

// After client.Connect(), the stream is open
// If you need to check if the stream is still healthy:
// - If Produce() or Run() returns an error, assume stream is dead
// - Otherwise, stream is live (will auto-reconnect on I/O failures per gRPC semantics)

// For graceful drain, cancel the context:
cancel()
err := client.Close()
```

---

## Technical Reference

### Producer Protocol

**Protocol Overview:**

1. Client opens stream
2. Client → Server: `Hello` with token
3. Server → Client: `HelloAck` with tenant_id, subject
4. Loop:
   - Client → Server: `CreateTask` or `CreateTaskBatch` with monotonically-increasing seq
   - Server → Client: `CreateAck` or `CreateAckBatch` (acks may arrive out of order)

**Proto Definition:** `internal/producer/proto/producerpb.proto`

**Key Messages:**

| Message | Direction | Purpose |
|---------|-----------|---------|
| `Hello` | Client→Server | Auth + stream setup |
| `HelloAck` | Server→Client | Auth success; return tenant_id, subject |
| `CreateTask` | Client→Server | Submit one task with seq |
| `CreateTaskBatch` | Client→Server | Submit N tasks in one message (Phase 6) |
| `CreateAck` | Server→Client | Ack for one CreateTask; echoes seq, task_id, or error |
| `CreateAckBatch` | Server→Client | Ack for all tasks in CreateTaskBatch |
| `ServerError` | Server→Client | Stream-level error (auth failure, etc.) |

**Task Fields:**

```protobuf
message CreateTask {
  uint64 seq = 1;                          // Producer-assigned, must be monotonic
  string command = 2;                      // Task type (e.g., "GENERATE_MASTER")
  bytes payload = 3;                       // Opaque JSON; server stores as-is
  int32 priority = 4;                      // 0-10; higher = earlier claim
  string webhook = 5;                      // Result webhook URL
  int32 max_attempts = 6;                  // Max retries before DLQ
  string idempotency_key = 7;              // For deduplication
  google.protobuf.Timestamp run_at = 8;    // When task becomes claimable
  int32 delay_seconds = 9;                 // Delay before claimable
  string trace_parent = 10;                // W3C trace context
  string trace_state = 11;
}
```

**Semantics:**

- Each `seq` must be strictly monotonically increasing per stream
- Server echoes `seq` in `CreateAck` so client can pair them without ordering dependency
- Multiple Produces can be in flight; acks may arrive out of order
- `run_at` and `delay_seconds`: at most one should be set

---

### Worker Protocol

**Protocol Overview:**

1. Client opens stream
2. Client → Server: `Hello` with token, worker_id
3. Server → Client: `HelloAck` with worker_id, tenant_id
4. Loop (per slot):
   - Client → Server: `Ready` with commands, lease_seconds, count
   - Server → Client: `TaskAssignment` or `TaskBatch`
   - Client processes task
   - Client → Server: `Result` or `ResultBatch`
   - Back to Ready

**Proto Definition:** `internal/worker/proto/workerpb.proto`

**Key Messages:**

| Message | Direction | Purpose |
|---------|-----------|---------|
| `Hello` | Client→Server | Auth + worker setup |
| `HelloAck` | Server→Client | Auth success; return worker_id, tenant_id |
| `Ready` | Client→Server | Ready for task(s); specify count for batching |
| `TaskAssignment` | Server→Client | Single task (count≤1) |
| `TaskBatch` | Server→Client | Multiple tasks (count>1) |
| `Result` | Client→Server | Task completed/failed (one task) |
| `ResultBatch` | Client→Server | Multiple results (Phase 6) |
| `Nack` | Client→Server | Requeue task after delay |
| `Heartbeat` | Client→Server | Extend lease on long-running task |
| `Abandon` | Client→Server | Release lease without marking failed |

**Result Status Values:**

- `COMPLETED`: Task succeeded; `result_json` field set
- `FAILED`: Task failed permanently; `error` field set
- Other statuses (created via `Nack`, `Heartbeat`, `Abandon`) are not Result status values

**Concurrency Model:**

```
Worker Stream (N slots)
  ├─ Slot 1: Ready → Task → Handle → Result → Ready → ...
  ├─ Slot 2: Ready → Task → Handle → Result → Ready → ...
  └─ Slot N: Ready → Task → Handle → Result → Ready → ...
```

Each slot runs independently in its own goroutine; failure in one slot does not block others.

**Task Model:**

```protobuf
message Task {
  string id = 1;
  string command = 2;
  bytes payload = 3;
  int32 priority = 4;
  string webhook = 5;
  int32 max_attempts = 6;
  string status = 7;                       // Current status (PENDING, IN_PROGRESS, etc.)
  string worker_id = 8;                    // Current lease owner
  string lease_until = 9;                  // ISO8601 timestamp
  int32 attempts = 10;                     // Number of attempts so far
  string tenant_id = 11;
  google.protobuf.Timestamp created_at = 12;
  google.protobuf.Timestamp updated_at = 13;
}
```

---

## Performance Explanation

### Why Streaming is Faster

codeQ's gRPC streaming APIs achieve 2-3× higher throughput than REST by addressing three bottlenecks:

#### 1. Single Authentication

**REST path:**
```
POST /tasks (auth + request processing + response) → RTT
POST /tasks (auth + request processing + response) → RTT
POST /tasks (auth + request processing + response) → RTT
```

**Streaming path:**
```
Hello (auth + tenant resolution) → RTT
Send CreateTask (no auth)
Send CreateTask (no auth)
Send CreateTask (no auth)
Recv Acks
```

**Impact:** REST amortizes ~1ms auth overhead per call; streaming amortizes once across all tasks.

#### 2. Async Pipelining

**REST (serial):**
```go
for i := 0; i < N; i++ {
    POST /tasks(i) // Blocks until response
}
// Total time: N × RTT + N × processing
```

**Streaming (pipelined):**
```go
for i := 0; i < N; i++ {
    go Produce(task(i))  // Returns immediately after Send (not after ack)
}
// All sends complete in ~1-2 gRPC frames
// Acks stream in asynchronously
// Total time: 1-2 × RTT + max(processing across all tasks)
```

**Impact:** Parallelizes I/O-bound operations.

#### 3. Batch Commits

**REST (N tasks):**
```
N separate POST calls
N separate HTTP status checks
N separate Redis writes (or fewer if coalesced, but still separate HTTP)
```

**Streaming with Batching (N tasks):**
```
One CreateTaskBatch message
Server spawns one fan-out goroutine (vs N event handlers)
One CreateAckBatch on the wire
Pebble write coalescer merges into fewer commits
```

**Impact:** Reduces gRPC framing overhead and server-side concurrency management.

### Benchmark Results

**Throughput (creates/second):**

| Scenario | REST | Streaming | Improvement |
|----------|------|-----------|-------------|
| Single producer, 32 concurrent Produce calls | ~3,000 ops/sec | ~15,000 ops/sec | 5× |
| Single producer, batch size 10 | ~2,500 ops/sec | ~33,000 ops/sec | 13× |
| Single worker, 32 concurrent slots | ~2,800 ops/sec | ~8,000 ops/sec | 3× |

**Latency (end-to-end create→claim→complete):**

- REST: ~3.2ms per cycle
- Streaming: ~1-2ms per cycle (in pipeline)

**Sustained load (k6):**

- REST: 1,000-2,000 req/s sustainable
- Streaming: 10,000+ req/s sustainable per instance

### When to Use Streaming

**Use streaming if:**

- You need >5,000 tasks/sec throughput
- P99 latency is critical (streaming has lower variance)
- Your producer/worker is CPU-bound (fewer goroutines contending for locks)
- You're running at cloud scale (fewer connections = lower NAT pressure)

**REST is fine if:**

- <5,000 tasks/sec
- You already have REST infrastructure
- Simplicity is more important than peak throughput
- Streaming complicates your deployment

---

## Examples

### Example: Producer Batch Pipeline

```go
func main() {
    client, err := producerclient.New(producerclient.Config{
        Addr:  "localhost:9092",
        Token: "producer-token",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()
    
    sess, err := client.Connect(context.Background())
    if err != nil {
        log.Fatal(err)
    }
    defer sess.Close()
    
    // Submit 100 tasks in batches of 10
    for batch := 0; batch < 10; batch++ {
        reqs := make([]producerclient.CreateRequest, 10)
        for i := 0; i < 10; i++ {
            reqs[i] = producerclient.CreateRequest{
                Command: "RENDER_VIDEO",
                Payload: []byte(fmt.Sprintf(`{"batch":%d,"item":%d}`, batch, i)),
                MaxAttempts: 3,
            }
        }
        
        results, err := sess.ProduceBatch(context.Background(), reqs)
        if err != nil {
            log.Printf("Batch %d failed: %v", batch, err)
            continue
        }
        
        successCount := 0
        for _, r := range results {
            if r.Err == nil {
                successCount++
            }
        }
        log.Printf("Batch %d: %d/%d succeeded", batch, successCount, len(reqs))
    }
}
```

### Example: Worker with Concurrency

```go
func main() {
    client, err := workerclient.New(workerclient.Config{
        Addr:        "localhost:9091",
        Token:       "worker-token",
        WorkerID:    "worker-1",
        Concurrency: 8,           // 8 parallel task slots
        BatchSize:   2,           // Claim 2 tasks per Ready
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()
    
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    
    go func() {
        sigCh := make(chan os.Signal, 1)
        signal.Notify(sigCh, syscall.SIGTERM)
        <-sigCh
        log.Println("Shutting down...")
        cancel()
    }()
    
    err = client.Run(ctx, handleTask)
    if err != nil && !errors.Is(err, context.Canceled) {
        log.Printf("Worker error: %v", err)
    }
}

func handleTask(ctx context.Context, task workerclient.Task) workerclient.Result {
    log.Printf("Processing %s (attempt %d/%d)", 
        task.Command, task.Attempts, task.MaxAttempts)
    
    // Simulate work
    select {
    case <-time.After(100 * time.Millisecond):
    case <-ctx.Done():
        return workerclient.Abandon(task.ID)
    }
    
    return workerclient.Completed(task.ID, []byte(`{"status":"ok"}`))
}
```

---

## Troubleshooting

### Producer

**Symptom**: `CreateAck` never arrives (Produce hangs)

**Causes:**
- Stream deadlock (rare). Solution: check server logs for errors
- Server is unreachable. Solution: verify network, TLS config
- Token invalid. Solution: check token, verify scopes

**Symptom**: `invalid command` error on every create

**Cause:** Command name not registered in task scheduler
**Solution:** Verify command name matches `eventTypes` in worker token or server config

---

### Worker

**Symptom**: Worker receives tasks but handler never completes

**Cause:** Context timeout during processing
**Solution:** Increase `LeaseSeconds` or reduce processing time

**Symptom**: High `task_id not found` errors on Result submission

**Cause:** Lease expired while handler was running
**Solution:** Increase `LeaseSeconds` or use `Heartbeat` to extend lease

**Symptom**: Worker stream closes with `UNAVAILABLE`

**Cause:** Server shutdown or network loss
**Solution:** Implement exponential backoff retry; connection will re-establish

---

## See Also

- [`docs/04-http-api.md`](./04-http-api.md): REST API reference
- [`docs/17-performance-tuning.md`](./17-performance-tuning.md): Performance optimization guide
- [`pkg/producerclient`](../pkg/producerclient/client.go): Producer client implementation
- [`pkg/workerclient`](../pkg/workerclient/client.go): Worker client implementation
- [`internal/producer/proto/producerpb.proto`](../internal/producer/proto/producerpb.proto): Producer protocol definition
- [`internal/worker/proto/workerpb.proto`](../internal/worker/proto/workerpb.proto): Worker protocol definition
