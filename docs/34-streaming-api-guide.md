# gRPC Streaming APIs

## Overview

codeQ provides two high-performance gRPC streaming APIs that replace the per-call HTTP round-trip overhead with long-lived bidirectional streams, achieving **2-3x throughput improvement** over REST:

1. **Producer Streaming API** - Submit tasks at scale (33k+ tasks/s per stream)
2. **Worker Streaming API** - Claim and complete tasks with concurrent slot processing

Both APIs authenticate once at stream-open, then pipeline requests without per-call authentication overhead.

---

## Producer Streaming API

### Overview

The producer streaming API replaces `POST /v1/codeq/tasks` with a single long-lived bidirectional gRPC stream. The client:

1. Opens a stream and sends `Hello` with a bearer token
2. Receives `HelloAck` with tenant_id and subject
3. Pipelines `CreateTask` events with monotonically-increasing sequence numbers
4. Receives `CreateAck` messages correlating back to requests

**Key benefit**: Multiple Produces from different goroutines can be in flight simultaneously, pipelined through a single stream.

### Tutorial: Basic Producer Stream

This tutorial shows how to create a producer stream client and submit tasks.

#### Step 1: Import and Configure

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
    cfg := producerclient.Config{
        Addr:  "localhost:9092",  // codeQ producer stream server
        Token: "your-bearer-token", // JWT or opaque bearer token
    }

    client, err := producerclient.New(cfg)
    if err != nil {
        log.Fatalf("failed to create client: %v", err)
    }
    defer client.Close()
}
```

#### Step 2: Connect and Open a Session

```go
    session, err := client.Connect(context.Background())
    if err != nil {
        log.Fatalf("failed to connect: %v", err)
    }
    defer session.Close()

    log.Printf("Connected to tenant %s as %s", session.TenantID(), session.Subject())
```

#### Step 3: Submit Tasks

```go
    // Submit a single task
    resp, err := session.Produce(context.Background(), &producerclient.CreateRequest{
        Command: "send-email",
        Payload: []byte(`{"to":"user@example.com"}`),
        Priority: 1,
        MaxAttempts: 3,
    })
    if err != nil {
        log.Fatalf("produce failed: %v", err)
    }

    log.Printf("Created task %s", resp.TaskID)
```

#### Step 4: Pipelined Submissions (Advanced)

Submit multiple tasks concurrently from different goroutines:

```go
    ctx := context.Background()
    results := make(chan *producerclient.CreateResponse, 100)

    // Submit 100 tasks concurrently
    for i := 0; i < 100; i++ {
        go func(idx int) {
            resp, err := session.Produce(ctx, &producerclient.CreateRequest{
                Command: "process-item",
                Payload: []byte(fmt.Sprintf(`{"id":%d}`, idx)),
            })
            if err != nil {
                log.Printf("task %d failed: %v", idx, err)
                return
            }
            results <- resp
        }(i)
    }

    // Collect results
    for i := 0; i < 100; i++ {
        resp := <-results
        log.Printf("Task created: %s", resp.TaskID)
    }
```

### How-To: Error Handling

The producer streaming API returns errors in `CreateResponse.Error`. Common cases:

```go
    resp, err := session.Produce(ctx, req)
    if err != nil {
        // Network or client-side error
        return fmt.Errorf("produce failed: %w", err)
    }

    // Check for server-side validation errors
    if resp.Error != "" {
        switch resp.Error {
        case "invalid-command":
            return fmt.Errorf("command not recognized")
        case "payload-too-large":
            return fmt.Errorf("payload exceeds max size")
        case "quota-exceeded":
            return fmt.Errorf("tenant task quota exceeded")
        default:
            return fmt.Errorf("create failed: %s", resp.Error)
        }
    }

    log.Printf("Task created: %s", resp.TaskID)
```

### How-To: TLS / mTLS

Use custom TLS credentials via `DialOptions`:

```go
    import "google.golang.org/grpc/credentials"

    // Load client certificate and key
    cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
    if err != nil {
        log.Fatalf("failed to load client cert: %v", err)
    }

    // Create TLS credentials
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
    }
    creds := credentials.NewTLS(tlsConfig)

    cfg := producerclient.Config{
        Addr:        "codeq.example.com:443",
        Token:       "bearer-token",
        DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(creds)},
    }

    client, err := producerclient.New(cfg)
    if err != nil {
        log.Fatalf("failed to create client: %v", err)
    }
```

### How-To: Batch Submissions (Phase 6)

For even higher throughput, batch multiple CreateTask messages into one:

```go
    // Built-in batch method (if supported by your version)
    // Creates TaskBatch internally
    tasks := []*producerclient.CreateRequest{
        {Command: "task1", Payload: []byte(`{"a":1}`)},
        {Command: "task2", Payload: []byte(`{"b":2}`)},
        {Command: "task3", Payload: []byte(`{"c":3}`)},
    }

    for _, task := range tasks {
        resp, err := session.Produce(context.Background(), task)
        // ... handle resp
    }

    // The client may internally batch these into a single CreateTaskBatch
```

### Reference: Producer Configuration

The `producerclient.Config` type controls behavior:

```go
type Config struct {
    // Addr is the gRPC dial target (e.g. "localhost:9092"). Required.
    Addr string

    // Token is the bearer token presented in Hello. Required.
    Token string

    // DialOptions are forwarded to grpc.NewClient for custom TLS/mTLS.
    // If empty, uses insecure transport (suitable for localhost development).
    DialOptions []grpc.DialOption

    // Logger receives structured info/warn/error events.
    // Defaults to slog.Default().
    Logger *slog.Logger
}
```

### Reference: CreateRequest Fields

Maps directly to the HTTP `POST /v1/codeq/tasks` body:

```go
type CreateRequest struct {
    Command        string    // Task command name (required)
    Payload        []byte    // Opaque JSON payload (required)
    Priority       int       // Task priority (0 = default, higher = first)
    Webhook        string    // URL for result delivery callback
    MaxAttempts    int       // Max claim attempts before failure (0 = server default)
    IdempotencyKey string    // For idempotent submissions (optional)
    RunAt          time.Time // Run the task at this time
    DelaySeconds   int       // Delay task start by N seconds
    TraceParent    string    // W3C trace context (optional)
}
```

### Explanation: Protocol Flow

```
Producer                                   Server
   │
   ├─── Hello(token) ───────────────────────>
   │                  <──── HelloAck ────────┤
   │              (tenant_id, subject)       │
   │                                          │
   ├─── CreateTask(seq=1) ──────────────────>
   │                      <── CreateAck ────┤
   │                    (seq=1, task_id)     │
   │                                          │
   ├─── CreateTask(seq=2) ──────────────────>
   │   (multiple requests in flight)         │
   ├─── CreateTask(seq=3) ──────────────────>
   │                      <── CreateAck ────┤
   │                    (seq=2, task_id)     │
   │                      <── CreateAck ────┤
   │                    (seq=3, task_id)     │
   │                                          │
   └──────────────────────────────────────────
```

The `seq` field lets the producer pair requests to responses without waiting for each ack.

### Explanation: Performance

**Throughput**: 33k+ tasks/sec per stream with pipelining.

**Latency**: ~5ms per create task when pipelined (vs ~20-30ms for REST due to per-call auth + HTTP middleware).

**Concurrency**: Use multiple streams for parallel submission workloads, or pipeline many tasks through a single stream via goroutines calling `Produce` concurrently.

---

## Worker Streaming API

### Overview

The worker streaming API replaces `POST /v1/codeq/workers/claim` and `POST /v1/codeq/tasks/{id}/result` with a single long-lived bidirectional gRPC stream. The worker:

1. Opens a stream and sends `Hello` with a bearer token
2. Receives `HelloAck` with worker_id and tenant_id
3. Sends `Ready` messages indicating capacity
4. Receives `Task` (or `TaskBatch`) assignments
5. Processes tasks with a user-provided Handler
6. Sends `Result`, `Nack`, `Abandon`, or `Heartbeat` messages back

**Key benefit**: Concurrent slots running independent claim-process-result cycles in parallel, with configurable concurrency.

### Tutorial: Basic Worker Stream

This tutorial shows how to create a worker stream client and process tasks.

#### Step 1: Import and Define a Handler

```go
package main

import (
    "context"
    "encoding/json"
    "log"

    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// Handler processes one task and returns the disposition
func handleTask(ctx context.Context, t workerclient.Task) workerclient.Result {
    log.Printf("Processing task %s with command %s", t.ID, t.Command)

    // Unmarshal payload
    var payload map[string]interface{}
    if err := json.Unmarshal(t.Payload, &payload); err != nil {
        return workerclient.Failed(t.ID, err.Error())
    }

    // Process the task
    if err := doWork(ctx, payload); err != nil {
        return workerclient.Failed(t.ID, err.Error())
    }

    // Return success
    result := map[string]interface{}{"status": "done"}
    resultJSON, _ := json.Marshal(result)
    return workerclient.Completed(t.ID, resultJSON)
}

func doWork(ctx context.Context, payload map[string]interface{}) error {
    // Your business logic here
    return nil
}
```

#### Step 2: Configure and Connect

```go
func main() {
    cfg := workerclient.Config{
        Addr:        "localhost:9091",       // codeQ worker stream server
        Token:       "your-bearer-token",
        WorkerID:    "worker-1",             // Identifies this worker instance
        Commands:    []string{"send-email"}, // Commands this worker handles
        Concurrency: 4,                      // Process 4 tasks in parallel
    }

    client, err := workerclient.New(cfg)
    if err != nil {
        log.Fatalf("failed to create client: %v", err)
    }
    defer client.Close()

    // Run the event loop (blocks until context is cancelled)
    if err := client.Run(context.Background(), handleTask); err != nil {
        log.Fatalf("worker loop failed: %v", err)
    }
}
```

#### Step 3: Process Tasks

The `client.Run` method:

1. Opens the stream and authenticates
2. Spawns N concurrent "slots", each running an independent Ready→Task→Result cycle
3. For each Task received, calls your Handler
4. Sends the Result back to the server
5. Repeats until context is cancelled or error occurs

### How-To: Graceful Shutdown

```go
    ctx, cancel := context.WithCancel(context.Background())

    // Run worker in a goroutine
    go func() {
        if err := client.Run(ctx, handleTask); err != nil {
            log.Printf("worker exited: %v", err)
        }
    }()

    // Wait for SIGTERM or similar
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGTERM)
    <-sigChan

    // Cancel context to stop the worker
    cancel()

    // Close client
    client.Close()
    log.Println("Worker shut down cleanly")
```

### How-To: Selective Command Processing

Restrict which commands your worker handles:

```go
    cfg := workerclient.Config{
        Addr:     "localhost:9091",
        Token:    "bearer-token",
        Commands: []string{"send-email", "send-sms"},
        // Only claim tasks with command in ["send-email", "send-sms"]
    }
```

To handle all commands, leave `Commands` empty (uses the token's eventTypes claim).

### How-To: Nack (Temporary Failure)

Requeue a task for later retry:

```go
    func handleTask(ctx context.Context, t workerclient.Task) workerclient.Result {
        if isTransientError {
            // Requeue after 30 seconds
            return workerclient.Nack(t.ID, 30)
        }
        // ...
    }
```

### How-To: Abandon (Release Without Requeue)

Release a task back to pending without nacking:

```go
    func handleTask(ctx context.Context, t workerclient.Task) workerclient.Result {
        if isShuttingDown {
            // Release the task; server returns it to pending immediately
            return workerclient.Abandon(t.ID)
        }
        // ...
    }
```

### How-To: Heartbeat (Extend Lease)

For long-running tasks, extend the lease:

```go
    func handleTask(ctx context.Context, t workerclient.Task) workerclient.Result {
        // Process in steps, heartbeat every 30 seconds
        for {
            select {
            case <-ctx.Done():
                return workerclient.Abandon(t.ID)
            case <-time.After(30 * time.Second):
                // Heartbeat extends lease by another 30s
                if err := workerclient.Heartbeat(t.ID, 30); err != nil {
                    log.Printf("heartbeat failed: %v", err)
                    return workerclient.Failed(t.ID, "heartbeat failed")
                }
            }
        }
    }
```

### How-To: Batch Mode (Phase 6)

For higher throughput, use batch mode to claim and submit results in batches:

```go
    cfg := workerclient.Config{
        Addr:        "localhost:9091",
        Token:       "bearer-token",
        WorkerID:    "worker-1",
        Concurrency: 4,
        BatchSize:   10,  // Claim up to 10 tasks per Ready
    }
    // Worker will:
    // - Send Ready{count=10}
    // - Receive up to 10 Tasks in TaskBatch
    // - After processing, send ResultBatch with all results in one message
```

### How-To: TLS / mTLS

Use custom TLS credentials:

```go
    import (
        "crypto/tls"
        "google.golang.org/grpc"
        "google.golang.org/grpc/credentials"
    )

    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caCertPool,
    }
    creds := credentials.NewTLS(tlsConfig)

    cfg := workerclient.Config{
        Addr:        "codeq.example.com:443",
        Token:       "bearer-token",
        DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(creds)},
    }
```

### Reference: Worker Configuration

The `workerclient.Config` type:

```go
type Config struct {
    // Addr is the gRPC dial target (e.g. "localhost:9091"). Required.
    Addr string

    // Token is the bearer token presented in Hello. Required.
    Token string

    // WorkerID identifies this worker for lease ownership.
    // If empty, the server uses the JWT subject.
    WorkerID string

    // Commands restricts what this worker pulls (e.g. ["send-email", "send-sms"]).
    // nil/empty means "use the token's eventTypes claim".
    Commands []string

    // Concurrency is the number of in-flight tasks (default: 1).
    // Each slot runs an independent Ready→Task→Result cycle.
    Concurrency int

    // LeaseSeconds is sent on each Ready (0 = server default).
    LeaseSeconds int

    // BatchSize controls task batching (Phase 6, default: 1):
    // - 0 or 1: Legacy single-task mode (one Ready → one Task → one Result)
    // - >1: Batch mode (Ready{count=BatchSize} → TaskBatch → ResultBatch)
    BatchSize int

    // IdleBackoff is how long a slot waits before re-sending Ready
    // when no task arrived (default: 50ms).
    IdleBackoff time.Duration

    // DialOptions are forwarded to grpc.NewClient (for TLS/mTLS).
    DialOptions []grpc.DialOption

    // Logger receives structured info/warn/error events.
    Logger *slog.Logger
}
```

### Reference: Task and Result Types

The `Task` type mirrors the REST API task object:

```go
type Task struct {
    ID         string    // Unique task ID
    Command    string    // Task command
    Payload    []byte    // Opaque JSON payload
    Priority   int       // Task priority
    Webhook    string    // Result webhook URL (optional)
    MaxAttempts int      // Max attempts before permanent failure
    Status     string    // Current status (e.g. "IN_PROGRESS")
    WorkerID   string    // Worker that claimed this task
    LeaseUntil string    // ISO8601 timestamp of lease expiration
    Attempts   int       // Current attempt number
    TenantID   string    // Tenant that owns this task
    CreatedAt  time.Time // Task creation time
    UpdatedAt  time.Time // Last update time
}
```

Result types are created with helper functions:

```go
// Submit successful completion
workerclient.Completed(taskID, resultJSON []byte) Result

// Submit permanent failure
workerclient.Failed(taskID, errorMessage string) Result

// Requeue for later (temporary failure)
workerclient.Nack(taskID, delaySecs int) Result

// Release back to pending (e.g. on shutdown)
workerclient.Abandon(taskID) Result

// Extend lease (for long-running tasks)
workerclient.Heartbeat(taskID, extendSecs int) Result
```

### Explanation: Concurrency Model

The worker spawns N **slots** (N = Config.Concurrency), each running independently:

```
Worker Stream Client
    │
    ├─ Slot 1: Ready → Task(A) → process → Result(A) → Ready → ...
    │
    ├─ Slot 2: Ready → Task(B) → process → Result(B) → Ready → ...
    │
    ├─ Slot 3: Ready → Task(C) → process → Result(C) → Ready → ...
    │
    └─ Slot 4: Ready → Task(D) → process → Result(D) → Ready → ...
```

Each slot:
1. Sends `Ready` with capacity for 1+ tasks (depending on BatchSize)
2. Receives Task(s) assignment
3. Calls your Handler(s)
4. Sends Result(s) back
5. Repeats until context is cancelled

Slots run in parallel; one blocked task doesn't block others.

### Explanation: Protocol Flow (Single-Task Mode)

```
Worker                                   Server
   │
   ├─── Hello(token) ───────────────────────>
   │                  <──── HelloAck ────────┤
   │                                          │
   ├─ Slot 1 ─┐
   │          ├─── Ready(commands) ────────>
   │          │            <── TaskAssignment
   │          │                         (task_A)
   │          ├─ [invoke handler] ──────────
   │          ├─── Result(task_A, status) →
   │          │            <── ResultAck ──┤
   │          └─── Ready(commands) ────────>
   │
   ├─ Slot 2 ─┐
   │          ├─── Ready(commands) ────────>
   │          │            <── TaskAssignment
   │          │                         (task_B)
   │          │
   │
```

### Explanation: Protocol Flow (Batch Mode)

```
Worker                                   Server
   │
   ├─── Hello(token) ───────────────────────>
   │
   ├─ Slot 1 ─┐
   │          ├─── Ready(count=10) ────────>
   │          │            <── TaskBatch ──┤
   │          │           (tasks: A, B, C) │
   │          ├─ [process A, B, C] ────────
   │          ├─── ResultBatch(3 results) ─>
   │          │            <── ResultAckBatch
   │          └─── Ready(count=10) ────────>
```

Batch mode amortizes gRPC framing and storage overhead across multiple tasks.

### Explanation: Performance

**Throughput**: 2-3x higher than REST due to:
- No per-call HTTP middleware overhead
- Single authentication
- Pipelined claim and result submission
- Batch amortization (Phase 6)

**Latency**: ~2-5ms per task claim and submit (vs ~10-20ms for REST).

**Concurrency**: Scale by:
- Increasing `Concurrency` (more parallel slots)
- Deploying multiple worker instances
- Using batch mode for higher throughput per slot

---

## Comparison: Streaming vs REST

| Aspect | Streaming | REST |
|--------|-----------|------|
| Throughput | 2-3x higher | Baseline |
| Latency | 2-5ms | 10-20ms |
| Per-call auth | ✓ Once | ✗ Every call |
| Pipelining | ✓ Native | ✗ Manual polling |
| Batch support | ✓ Phase 6 | ✗ No |
| Learning curve | Moderate | Minimal |
| Debugging | Logs + gRPC tools | HTTP tools + logs |
| Best for | High-throughput workloads | Simple scripts, webhooks |

---

## Migration Guide: REST to Streaming

### Producer: POST /tasks → Producer Stream

**Before (REST)**:

```go
    for i := 0; i < 100; i++ {
        resp, err := http.Post(
            "http://localhost:8080/v1/codeq/tasks",
            "application/json",
            bytes.NewReader(taskJSON),
        )
        // ... handle err
    }
```

**After (Streaming)**:

```go
    client, _ := producerclient.New(producerclient.Config{
        Addr:  "localhost:9092",
        Token: "bearer-token",
    })
    defer client.Close()

    session, _ := client.Connect(context.Background())
    defer session.Close()

    for i := 0; i < 100; i++ {
        resp, err := session.Produce(context.Background(), &producerclient.CreateRequest{
            Command: "my-command",
            Payload: taskPayload,
        })
        // ... handle err
    }
    // Results come back pipelined; can process while still submitting
```

### Worker: Poll REST → Worker Stream

**Before (REST - polling)**:

```go
    for {
        // Claim a task
        resp, _ := http.Post("http://localhost:8080/v1/codeq/workers/claim", ...)
        task := parseTask(resp)

        // Process
        result := handleTask(task)

        // Submit result
        http.Post(fmt.Sprintf("http://localhost:8080/v1/codeq/tasks/%s/result", task.ID), ...)

        time.Sleep(100 * time.Millisecond) // Poll interval
    }
```

**After (Streaming)**:

```go
    client, _ := workerclient.New(workerclient.Config{
        Addr:        "localhost:9091",
        Token:       "bearer-token",
        Concurrency: 4,
    })
    defer client.Close()

    client.Run(context.Background(), func(ctx context.Context, t workerclient.Task) workerclient.Result {
        result := handleTask(t)
        return result
        // No need to manually submit; client sends automatically
    })
```

---

## Server Configuration

Enable streaming endpoints in `codeq.yml`:

```yaml
# Producer stream gRPC server (claims only, no HTTP)
producer_stream_addr: ":9092"

# Worker stream gRPC server (claims only, no HTTP)
worker_stream_addr: ":9091"
```

Both are **optional**; if not set, only REST endpoints are available.

---

## Troubleshooting

### Connection Refused

```
Error: dial localhost:9092: connection refused
```

**Cause**: Streaming server not running or listening on wrong port.

**Fix**: 
- Check `codeq.yml` has `producer_stream_addr` or `worker_stream_addr` set
- Verify server is running: `netstat -an | grep 9092`
- Check server logs for startup errors

### Authentication Failed

```
Error: rpc error: code = Unauthenticated desc = invalid token
```

**Cause**: Bearer token invalid or expired.

**Fix**:
- Verify token format (JWT or opaque bearer token)
- Ensure token has required scopes
- Check server's auth plugin configuration
- Review server logs for token validation errors

### TaskBatch / ResultBatch Not Working

```
Error: request has been dropped, as the server is shutting down
```

**Cause**: Server version doesn't support batching (Phase 6).

**Fix**:
- Set `BatchSize: 1` in worker config (or omit it)
- Set `BatchSize: 0` in producer config
- Upgrade to latest codeQ version

### High Latency in Batch Mode

**Cause**: Batch size too small, or server queue is empty.

**Fix**:
- Increase `BatchSize` to 10-50 to amortize overhead
- Monitor queue depth; if consistently empty, reduce batch size
- Profile with `pprof` to identify hot paths

---

## Related Documentation

- **Architecture**: [03-architecture.md](03-architecture.md) – System design and components
- **HTTP API**: [04-http-api.md](04-http-api.md) – REST endpoints (for comparison)
- **Performance**: [17-performance-tuning.md](17-performance-tuning.md) – Performance tips and benchmarks
- **Example Applications**: [examples/](../examples/) – Full working examples
- **Integration Guides**: [sdks/README.md](../sdks/README.md) – Language-specific SDKs
