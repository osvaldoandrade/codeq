# gRPC Streaming APIs

codeQ provides high-throughput gRPC streaming APIs for both producers and workers, enabling 2-3x throughput improvement over REST by amortizing authentication and enabling async pipelining. This guide covers both the producer streaming API (for task submission) and the worker streaming API (for task claiming and result submission).

## Overview

The streaming APIs replace high-frequency REST calls with a single long-lived bidirectional gRPC stream:

- **Producer Streaming**: eliminates per-call HTTP round-trip on the create hot path
- **Worker Streaming**: eliminates per-call HTTP round-trips on the claim and result hot paths

Authentication and tenant resolution happen exactly once at stream-open; subsequent messages inherit the resolved tenant and cannot override it.

## Producer Streaming API

### Protocol Flow

1. **Client opens stream** → Server establishes bidirectional connection
2. **Client sends Hello** with bearer token
3. **Server responds HelloAck** with tenant_id and subject
4. **Client pipelines CreateTask messages** with monotonically-increasing seq
5. **Server acks each** with CreateAck (echoing seq, task_id, or error_message)
6. **Stream stays open** for new CreateTask submissions

### Architecture

The producer streaming protocol is fully asynchronous—a producer may have N requests in flight before the first ack arrives. The `seq` field lets the producer correlate acks back to its own requests without round-tripping.

```
Producer                           Server
   |                                 |
   |------------ Open stream ------->|
   |                                 |
   |----------- Hello (token) ------>|
   |                                 |
   |<-------- HelloAck (tenant) ------|
   |                                 |
   |-- CreateTask(seq=1) ----------->|
   |-- CreateTask(seq=2) ----------->|
   |-- CreateTask(seq=3) ----------->|
   |<-- CreateAck(seq=2, task_id) ----|
   |<-- CreateAck(seq=1, task_id) ----|
   |<-- CreateAck(seq=3, task_id) ----|
   |                                 |
```

### Producer Streaming Tutorial

#### Step 1: Set Up Your Environment

````go
import (
    "context"
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)
````

#### Step 2: Create a Producer Client

````go
client, err := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092", // gRPC streaming server address
    Token: "your-producer-token",
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
````

#### Step 3: Connect and Open a Session

````go
ctx := context.Background()
session, err := client.Connect(ctx)
if err != nil {
    log.Fatal(err)
}
````

#### Step 4: Submit Tasks (with pipelining)

The `Produce` method is safe for concurrent invocation across multiple goroutines:

````go
// Send tasks from multiple goroutines
for i := 0; i < 100; i++ {
    go func(idx int) {
        taskID, err := session.Produce(ctx, &producerclient.CreateTaskRequest{
            Command: "PROCESS_ORDER",
            Payload: []byte(fmt.Sprintf(`{"orderId": "order-%d"}`, idx)),
            Priority: 5,
        })
        if err != nil {
            log.Printf("Produce failed: %v", err)
        } else {
            log.Printf("Created task %s", taskID)
        }
    }(i)
}
````

The `Produce` call blocks only until the matching `CreateAck` arrives, allowing pipelining of multiple requests.

### Producer Streaming How-To: Batch Submissions

For bulk submissions, use concurrent goroutines to pipeline requests:

````go
type SubmitResult struct {
    OrderID string
    TaskID  string
    Error   error
}

results := make(chan SubmitResult, 100)

// Submit 100 orders in parallel
for _, order := range orders {
    go func(o Order) {
        taskID, err := session.Produce(ctx, &producerclient.CreateTaskRequest{
            Command: "PROCESS_ORDER",
            Payload: []byte(fmt.Sprintf(`{"orderId": "%s"}`, o.ID)),
            IdempotencyKey: fmt.Sprintf("order-%s", o.ID),
        })
        results <- SubmitResult{o.ID, taskID, err}
    }(order)
}

// Collect results
for i := 0; i < 100; i++ {
    result := <-results
    if result.Error != nil {
        log.Printf("Order %s submission failed: %v", result.OrderID, result.Error)
    } else {
        log.Printf("Order %s → task %s", result.OrderID, result.TaskID)
    }
}
````

### Producer Streaming Technical Reference

#### Message: Hello

Sent by client as the first message after stream open.

- `token` (string, required): Bearer token for authentication

**Example:**
````proto
message Hello {
  string token = 1;
}
````

#### Message: CreateTask

Producer submits one task per CreateTask message.

- `seq` (uint64, required): Client-assigned sequence number, must be strictly monotonically increasing
- `command` (string, required): Queue command identifier
- `payload` (bytes, required): Opaque JSON task data (stored server-side as-is)
- `priority` (int32, optional): Task priority (0-100)
- `webhook` (string, optional): Result callback URL
- `max_attempts` (int32, optional): Max retry count
- `idempotency_key` (string, optional): Deduplication key
- `run_at` (Timestamp, optional): When task becomes claimable (RFC3339)
- `delay_seconds` (int32, optional): Delay before task becomes claimable
- `trace_parent` (string, optional): W3C trace context parent span ID
- `trace_state` (string, optional): W3C trace state

**Example:**
````proto
message CreateTask {
  uint64 seq = 1;
  string command = 2;
  bytes payload = 3;
  int32 priority = 4;
  string webhook = 5;
  int32 max_attempts = 6;
  string idempotency_key = 7;
  google.protobuf.Timestamp run_at = 8;
  int32 delay_seconds = 9;
  string trace_parent = 10;
  string trace_state = 11;
}
````

#### Message: HelloAck

Server response to Hello.

- `tenant_id` (string): Resolved tenant identifier
- `subject` (string): JWT subject claim (usually user ID)

#### Message: CreateAck

Server response to CreateTask. Pairs with the CreateTask whose `seq` matches.

- `seq` (uint64): Echoes the seq from CreateTask for request-response pairing
- `ok` (bool): Success flag
- `task_id` (string): Server-assigned task ID (present if ok=true)
- `error_message` (string): Error reason (present if ok=false)

### Producer Streaming Error Handling

Common error responses in CreateAck:

- `invalid_command`: Command not recognized
- `payload_too_large`: Payload exceeds size limit
- `duplicate_idempotency_key`: Idempotency key collision (returns existing task_id)
- `invalid_webhook_url`: Webhook URL format invalid
- `authentication_failed`: Bearer token validation failed (disconnects stream)
- `rate_limit_exceeded`: Rate limiting applied

If authentication fails, the server closes the stream with an Unauthenticated status.

### Producer Streaming Performance

**Throughput improvement**: 2-3x vs REST API

- REST API ceiling: ~33k creates/sec per connection
- Streaming API: ~100k+ creates/sec per connection

**Latency**: p99 latency reduced by 50-60% under high concurrency due to:
- Single auth round-trip amortized across many requests
- Async ack model eliminates per-request round-trip wait
- HTTP/2 multiplexing reduces network overhead

## Worker Streaming API

### Protocol Flow

1. **Client opens stream** → Server establishes bidirectional connection
2. **Client sends Hello** with bearer token and worker_id
3. **Server responds HelloAck** with worker_id and tenant_id
4. **Each worker "slot"** independently loops:
   - Send **Ready** (declare capacity for commands)
   - Receive **Task** (server assigns one task when available)
   - Process the task
   - Send **Result** / **Nack** / **Abandon** / **Heartbeat**
   - Receive ack and repeat
5. **Multiple slots run in parallel** (configurable concurrency)

### Architecture

The worker streaming protocol supports configurable concurrency. Multiple "slots" (typically 5-10) run independent Ready→Task→Result cycles in parallel. Slots run concurrently; one failure doesn't block others.

```
Worker                           Server
   |                               |
   |------- Open stream ----->|
   |                               |
   |---- Hello (token) ------>|
   |                               |
   |<----- HelloAck ----------|
   |                               |
   |-- Ready(commands) ------>|
   |-- Ready(commands) ------>|  (2 concurrent slots)
   |-- Ready(commands) ------>|
   |                               |
   |<----- Task(id=123) ------|
   |<----- Task(id=456) ------|
   |<----- Task(id=789) ------|
   |                               |
   | (worker processes in parallel)|
   |                               |
   |-- Result(id=123) ------>|
   |-- Result(id=456) ------>|
   |-- Result(id=789) ------>|
   |                               |
   |<----- ResultAck ---------|
   |<----- ResultAck ---------|
   |<----- ResultAck ---------|
   |                               |
   |-- Ready ----------------->|  (refill slots)
   |                               |
```

### Worker Streaming Tutorial

#### Step 1: Define Your Task Handler

````go
import (
    "context"
    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func handleOrder(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Unmarshal payload
    var order struct {
        ID    string `json:"id"`
        Total float64 `json:"total"`
    }
    if err := json.Unmarshal(task.Payload, &order); err != nil {
        return workerclient.Failed(task.ID, fmt.Sprintf("invalid payload: %v", err))
    }

    // Process order
    if order.Total < 0 {
        return workerclient.Failed(task.ID, "invalid order total")
    }

    // Simulate processing
    time.Sleep(100 * time.Millisecond)

    // Return success with optional result data
    result := map[string]string{"status": "shipped", "trackingId": "tk-123"}
    data, _ := json.Marshal(result)
    return workerclient.Completed(task.ID, data)
}
````

#### Step 2: Create a Worker Client

````go
client, err := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091", // gRPC streaming server address
    Token:       "your-worker-token",
    WorkerID:    "worker-1",
    Commands:    []string{"PROCESS_ORDER"},
    Concurrency: 5, // Process up to 5 tasks concurrently
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
````

#### Step 3: Start Processing Tasks

````go
ctx := context.Background()
if err := client.Run(ctx, handleOrder); err != nil {
    log.Fatal(err)
}
````

The `Run` method blocks indefinitely, continuously:
1. Sending Ready messages
2. Receiving Task assignments
3. Invoking your Handler concurrently (up to Concurrency limit)
4. Sending Result/Nack/Abandon based on handler return value

### Worker Streaming How-To: Advanced Error Handling

Use `Nack` to requeue tasks with backoff:

````go
func handleTaskWithRetry(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Attempt to process
    if err := processTask(task); err != nil {
        if isTransientError(err) {
            // Transient error: requeue with exponential backoff
            delaySeconds := 10 * (1 << uint(task.Attempts)) // 2^attempts * 10
            if delaySeconds > 3600 {
                delaySeconds = 3600 // Cap at 1 hour
            }
            return workerclient.Nack(task.ID, delaySeconds, fmt.Sprintf("transient error: %v", err))
        }
        // Permanent error: fail and let DLQ handle it
        return workerclient.Failed(task.ID, fmt.Sprintf("permanent error: %v", err))
    }
    return workerclient.Completed(task.ID, nil)
}
````

### Worker Streaming How-To: Heartbeat for Long-Running Tasks

Extend the lease on tasks that take longer than expected:

````go
func handleLongTask(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Start a goroutine to send heartbeats every 30 seconds
    heartbeatTicker := time.NewTicker(30 * time.Second)
    defer heartbeatTicker.Stop()

    go func() {
        for range heartbeatTicker.C {
            // Extend lease by 60 seconds
            client.Heartbeat(ctx, task.ID, 60)
        }
    }()

    // Do the actual work (might take 10+ minutes)
    result := doLongWork(ctx, task)

    return result
}
````

### Worker Streaming How-To: Graceful Shutdown

Abandon tasks on shutdown to prevent lease timeout:

````go
func startWorker(client *workerclient.Client, handler workerclient.Handler) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Trap SIGTERM
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGTERM)

    go func() {
        <-sigChan
        log.Println("Shutting down gracefully...")
        cancel() // Stop accepting new tasks
        time.Sleep(5 * time.Second) // Allow in-flight tasks to finish
        client.Close()
    }()

    if err := client.Run(ctx, handler); err != nil && err != context.Canceled {
        log.Fatal(err)
    }
}
````

### Worker Streaming Technical Reference

#### Message: Hello

Sent by client as the first message after stream open.

- `token` (string, required): Bearer token for authentication
- `worker_id` (string, optional): Identifies this worker for lease ownership; if empty the server uses JWT subject

#### Message: Ready

Declares capacity for one task matching specified commands.

- `commands` (string[], optional): Filter to these commands; if empty, worker accepts any command the token allows
- `lease_seconds` (int32, optional): How many seconds to hold the lease; 0 means server default

Each Ready consumes exactly one Task. If the worker wants prefetching (N tasks in flight), it sends N Readys.

#### Message: Result

Worker submits completion for a task.

- `task_id` (string, required): ID of the completed task
- `status` (string, required): "COMPLETED" or "FAILED"
- `result_json` (bytes, optional): Present when status="COMPLETED"; opaque JSON result data
- `error` (string, optional): Present when status="FAILED"; error message

#### Message: Nack

Returns the task to the queue with optional delay.

- `task_id` (string, required): ID of the task to requeue
- `delay_seconds` (int32, optional): How long to delay before task becomes claimable again (0 = immediately)
- `reason` (string, optional): Human-readable reason for nack

#### Message: Heartbeat

Extends the worker's lease on a task it's still processing.

- `task_id` (string, required): ID of the task
- `extend_seconds` (int32, required): How many more seconds to hold the lease

#### Message: Abandon

Releases the lease without nacking—task goes straight back to pending.

- `task_id` (string, required): ID of the task

#### Message: HelloAck

Server response to Hello.

- `worker_id` (string): Confirmed worker identifier
- `tenant_id` (string): Resolved tenant identifier

#### Message: TaskAssignment

Server sends one task per Ready.

- `task` (Task): The assigned task (mirrors REST API task model)

Task fields:
- `id` (string): Server-assigned task ID
- `command` (string): Queue command
- `payload` (bytes): Task data
- `priority` (int32): Task priority
- `webhook` (string): Callback URL
- `max_attempts` (int32): Max retry count
- `status` (string): Current status
- `worker_id` (string): Claimed by this worker
- `lease_until` (string): RFC3339 timestamp when lease expires
- `attempts` (int32): Number of attempts so far
- `tenant_id` (string): Task's tenant
- `created_at` (Timestamp): Task creation time
- `updated_at` (Timestamp): Last update time

#### Message: ResultAck / NackAck / HeartbeatAck / AbandonAck

Server response to Result/Nack/Heartbeat/Abandon.

- `task_id` (string): ID of the task
- `ok` (bool): Success flag
- `error_message` (string): Error reason if ok=false
- `applied_delay_seconds` (int32): Actual delay applied (NackAck only)
- `dlq` (bool): Task moved to DLQ (NackAck only, if max_attempts exceeded)

### Worker Streaming Error Handling

Common ack errors:

- `not-found`: Task not found (already completed or deleted)
- `not-owner`: Another worker holds the lease
- `not-in-progress`: Task not in progress (already completed)
- `invalid_status`: Status must be "COMPLETED" or "FAILED"
- `authentication_failed`: Bearer token validation failed (disconnects stream)

The `requeueExpired` optimization distinguishes errors:

- **Semantic errors** (`not-in-progress`, `not-found`): Task moved/completed elsewhere → safe to clean stale inprog entry
- **Infrastructure errors** (other errors): Retry with backoff

This prevents resurrection of completed tasks under weak isolation.

### Worker Streaming Performance

**Throughput improvement**: 2-3x vs REST API

- REST claim ceiling: ~10k claims/sec per connection
- Streaming claim ceiling: ~30k+ claims/sec per connection
- REST result ceiling: ~8k results/sec per connection
- Streaming result ceiling: ~25k+ results/sec per connection

**Latency**: p99 latency reduced by 40-50% under load due to:
- Single auth round-trip amortized across many operations
- HTTP/2 multiplexing and persistent connection
- Concurrent slots eliminate per-slot blocking

## Explanation: When to Use Streaming vs REST

### Use Streaming APIs if:

- **High throughput requirements**: Creating or claiming >1k tasks/sec
- **Latency-sensitive applications**: Sub-50ms p99 latency critical
- **Long-running workers**: Processes many tasks per connection session
- **Producer batching**: Submit many tasks in rapid succession

### Use REST APIs if:

- **Occasional submissions**: <100 tasks/sec
- **One-off operations**: Single task or claim then disconnect
- **Legacy integration**: Existing HTTP-only infrastructure
- **Simplified client logic**: REST libraries more mature in your ecosystem

### Hybrid Approach:

Mix both APIs in the same application:
- Use REST for admin operations (cleanup, stats)
- Use REST for one-off task creations
- Use streaming for high-throughput producer batches
- Use streaming for worker main loops

## Configuration

Both gRPC streaming servers are optional and disabled by default.

### Environment Variables

**Producer Streaming:**
- `PRODUCER_STREAM_ADDR`: gRPC listener address (e.g., `0.0.0.0:9092`). If unset, producer streaming is disabled.

**Worker Streaming:**
- `WORKER_STREAM_ADDR`: gRPC listener address (e.g., `0.0.0.0:9091`). If unset, worker streaming is disabled.

### Docker Compose Example

````yaml
services:
  codeq:
    environment:
      PRODUCER_STREAM_ADDR: 0.0.0.0:9092
      WORKER_STREAM_ADDR: 0.0.0.0:9091
    ports:
      - "9091:9091"  # Worker streaming
      - "9092:9092"  # Producer streaming
````

## TLS/mTLS Support

Both streaming servers support TLS and mTLS for production deployments.

### Client-Side Configuration

Pass gRPC dial options to enable TLS:

**Producer:**
````go
import "google.golang.org/grpc/credentials"

creds, err := credentials.NewClientTLSFromFile(
    "path/to/ca.crt",
    "codeq.example.com",
)
if err != nil {
    log.Fatal(err)
}

client, err := producerclient.New(producerclient.Config{
    Addr:  "codeq.example.com:9092",
    Token: "token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(creds),
    },
})
````

**Worker:**
````go
creds, err := credentials.NewClientTLSFromFile(
    "path/to/ca.crt",
    "codeq.example.com",
)
if err != nil {
    log.Fatal(err)
}

client, err := workerclient.New(workerclient.Config{
    Addr:  "codeq.example.com:9091",
    Token: "token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(creds),
    },
})
````

## Examples

### Complete Producer Example: Batch Submission

See `examples/producer-streaming/batch.go` for a complete working example that:
- Opens a streaming session
- Submits 1000 tasks concurrently
- Tracks success/error rate
- Measures throughput

Run with:
````bash
go run examples/producer-streaming/batch.go
````

### Complete Worker Example: Task Processing

See `examples/worker-streaming/worker.go` for a complete working example that:
- Connects with 5 concurrent slots
- Processes mock tasks
- Demonstrates error handling
- Shows graceful shutdown

Run with:
````bash
go run examples/worker-streaming/worker.go
````

## SDK Support

Official client SDKs are available for:

- **Go**: `github.com/osvaldoandrade/codeq/pkg/producerclient` and `github.com/osvaldoandrade/codeq/pkg/workerclient` (included in this repo)
- **Node.js/TypeScript**: `@osvaldoandrade/codeq-streaming` (separate repo, coming soon)
- **Java**: `com.example.codeq:codeq-streaming` (separate repo, coming soon)
- **Python**: `codeq-streaming` (separate repo, coming soon)

## Cross-References

- [HTTP API Reference](04-http-api.md): REST-based task operations (alternative to producer streaming)
- [Architecture](03-architecture.md): System design and component interactions
- [Worker Streaming Implementation](../pkg/workerclient/client.go): Client source code
- [Producer Streaming Implementation](../pkg/producerclient/client.go): Client source code
- [Performance Tuning](17-performance-tuning.md): Optimization strategies for both APIs
