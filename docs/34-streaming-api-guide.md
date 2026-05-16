# Streaming API Guide

codeQ Phase 3 provides gRPC streaming APIs for producers and workers to bypass HTTP round-trip overhead and achieve higher throughput. This guide covers both the producer and worker streaming SDKs.

## Overview: REST vs Streaming

**HTTP REST API** (`POST /v1/codeq/tasks`):
- Per-call auth and tenant validation
- ~33k creates/sec ceiling per replica
- Simple request-response model
- Good for: occasional task creation, simple integrations

**gRPC Streaming API**:
- One-time auth at stream open
- Async pipelining (many requests in-flight before first ack)
- Bidirectional streams
- ~33k+ creates/sec with concurrent pipelining
- Good for: high-throughput producers, batch operations, low-latency requirements

## Producer Streaming API

### Concepts

**Client**: Represents a gRPC connection to the codeQ producer server. Reusable across multiple sessions.

**Session**: One authenticated bidirectional stream. Multiple goroutines can call `Produce` concurrently.

**Sequence Numbers**: Each `Produce` call gets an incrementing seq number. Server echoes this in the Ack, letting you correlate responses asynchronously.

### Getting Started

#### 1. Create a Client

````go
import "github.com/osvaldoandrade/codeq/pkg/producerclient"

client, err := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092",  // gRPC server address (required)
    Token: "my-bearer-token", // Bearer token for auth (required)
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
````

#### 2. Connect and Open a Session

````go
ctx := context.Background()
session, err := client.Connect(ctx)
if err != nil {
    log.Fatal(err)
}
defer session.Close()
````

The `Connect` call:
1. Opens a bidirectional gRPC stream
2. Sends a `Hello` message with your bearer token
3. Receives `HelloAck` with tenant ID and subject
4. Starts a background reader goroutine to receive `CreateAck` messages

#### 3. Produce (Send) Tasks

````go
taskID, err := session.Produce(ctx, producerclient.CreateRequest{
    Command:        "GENERATE_MASTER",
    Payload:        []byte(`{"jobId":"j-123"}`),
    Priority:       5,
    Webhook:        "https://example.org/hook",
    MaxAttempts:    8,
    IdempotencyKey: "job-j-123",
    RunAt:          time.Now().Add(1 * time.Hour),
})
if err != nil {
    log.Printf("Failed to create task: %v", err)
}
log.Printf("Created task: %s", taskID)
````

**Fields in `CreateRequest`**:
- `Command` (required): Queue command name
- `Payload` (required): Task payload as JSON bytes
- `Priority` (optional, default 0): Integer priority for queue ordering
- `Webhook` (optional): URL to POST result on completion
- `MaxAttempts` (optional, default 5): Max retry attempts
- `IdempotencyKey` (optional): Deduplication key (24h TTL)
- `RunAt` (optional): RFC3339 timestamp when task becomes claimable
- `DelaySeconds` (optional): Convenience relative delay (alt to `RunAt`)
- `TraceParent` (optional): W3C trace context for distributed tracing
- `TraceState` (optional): W3C trace state

**Behavior**:
- `Produce` blocks until the matching `CreateAck` arrives (or context cancelled)
- Returns `taskID` on success (status 202)
- Returns error if creation rejected or stream closed
- Safe to call concurrently from multiple goroutines

### Concurrency Example

````go
import (
    "sync"
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

client, _ := producerclient.New(cfg)
defer client.Close()

session, _ := client.Connect(ctx)
defer session.Close()

// Dispatch tasks from multiple goroutines
var wg sync.WaitGroup
for i := 0; i < 100; i++ {
    wg.Add(1)
    go func(idx int) {
        defer wg.Done()
        payload := []byte(fmt.Sprintf(`{"job":%d}`, idx))
        taskID, err := session.Produce(ctx, producerclient.CreateRequest{
            Command:   "GENERATE_MASTER",
            Payload:   payload,
            Priority:  idx % 10,
        })
        if err != nil {
            log.Printf("Task %d failed: %v", idx, err)
        } else {
            log.Printf("Task %d created: %s", idx, taskID)
        }
    }(i)
}
wg.Wait()
````

### Error Handling

#### Connection Errors

If the connection fails or stream closes:
- Pending `Produce` calls unblock immediately with error
- `Session.Close()` is safe to call multiple times
- Create a new `Session` to reconnect

````go
taskID, err := session.Produce(ctx, req)
if err != nil {
    if errors.Is(err, context.Canceled) {
        // Context was cancelled
    } else {
        log.Printf("Stream error: %v", err)
        // Reconnect
        session.Close()
        session, _ = client.Connect(ctx)
    }
}
````

#### Validation Errors

Some `Produce` calls fail due to validation (invalid command, etc):

````go
taskID, err := session.Produce(ctx, producerclient.CreateRequest{
    Command: "INVALID_COMMAND",
    Payload: []byte("..."),
})
if err != nil {
    // Validation error (not a stream error)
    log.Printf("Rejected: %v", err)
}
````

The error is returned without closing the stream. Other pending produces are unaffected.

### TLS / mTLS Configuration

For production deployments, enable TLS:

````go
import "google.golang.org/grpc/credentials"

tlsCreds, err := credentials.NewClientTLSFromFile(
    "/path/to/ca.pem",
    "codeq.example.org",
)
if err != nil {
    log.Fatal(err)
}

client, err := producerclient.New(producerclient.Config{
    Addr:   "codeq.example.org:9092",
    Token:  "my-token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(tlsCreds),
    },
})
````

For mutual TLS (mTLS), include client certificate and key:

````go
creds, err := credentials.NewClientTLSFromFile(
    "/path/to/ca.pem",
    "codeq.example.org",
)
if err != nil {
    log.Fatal(err)
}

cert, err := tls.LoadX509KeyPair(
    "/path/to/client-cert.pem",
    "/path/to/client-key.pem",
)
if err != nil {
    log.Fatal(err)
}

tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{cert},
    RootCAs:      caCertPool,
    ServerName:   "codeq.example.org",
}

client, _ := producerclient.New(producerclient.Config{
    Addr:   "codeq.example.org:9092",
    Token:  "my-token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    },
})
````

### Logging and Observability

Pass a structured logger to track client lifecycle:

````go
import "log/slog"

logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

client, _ := producerclient.New(producerclient.Config{
    Addr:   "localhost:9092",
    Token:  "token",
    Logger: logger,
})
// Client logs debug/info/warn/error events to logger
````

Log output includes:
- `producerclient: hello ok` (tenant_id, subject)
- `producerclient: stream closed` (on reader goroutine exit)
- `producerclient: server error event` (code, message)

---

## Worker Streaming API

### Concepts

**Client**: Represents a gRPC connection to the codeQ worker server.

**Handler**: Your user-defined function that processes one task. Must be concurrent-safe (called in parallel slots).

**Slot**: Independent concurrent context within `Client.Run`. Each slot runs Ready→Claim→Task→Handler→Result cycle.

**Result**: Disposition of task (Completed, Failed, Nack, Abandon).

### Getting Started

#### 1. Define a Handler

The handler receives a task and returns a result. It can be called from multiple goroutines:

````go
import (
    "context"
    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func myHandler(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Do work...
    log.Printf("Processing %s: %s", task.Command, task.ID)
    
    // Parse payload
    var payload map[string]any
    json.Unmarshal(task.Payload, &payload)
    
    // Simulate work
    select {
    case <-time.After(1 * time.Second):
        // Success
        return workerclient.Completed(map[string]any{
            "status":  "done",
            "jobId":   payload["jobId"],
        })
    case <-ctx.Done():
        // Context cancelled (task lease expiring or worker shutting down)
        return workerclient.Abandon()
    }
}
````

#### 2. Create a Client

````go
client, err := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",        // gRPC server address (required)
    Token:       "my-bearer-token",       // Bearer token (required)
    Concurrency: 10,                      // Parallel task slots (optional, default 1)
    Commands:    []string{"GENERATE_MASTER"}, // Command filter (optional)
    LeaseSeconds: 30,                     // Task lease duration (optional, server default if 0)
})
if err != nil {
    log.Fatal(err)
}
defer client.Close()
````

#### 3. Run the Worker Loop

````go
ctx := context.Background()
err := client.Run(ctx, myHandler)
if err != nil {
    log.Fatal(err)
}
````

`client.Run` blocks until the context is cancelled or stream closes. It:
1. Opens a bidirectional gRPC stream
2. Authenticates with `Hello` → `HelloAck`
3. Runs N concurrent slots (N = Concurrency config)
4. Each slot sends `Ready`, receives `Task`, calls handler, sends `Result`
5. Returns when context cancelled

### Task Result Types

#### Completed: Task Done

Mark the task successfully completed:

````go
return workerclient.Completed(map[string]any{
    "output": "result data",
    "status": "done",
})
````

The payload is JSON-encoded and stored as the task result. Can be nil if no payload needed:

````go
return workerclient.Completed(nil)
````

#### Failed: Permanent Failure

Mark the task as permanently failed:

````go
return workerclient.Failed("database connection timeout after 3 retries")
````

Behavior:
- Respects `MaxAttempts`: if attempt count reaches MaxAttempts, task goes to DLQ
- Otherwise, task is moved to delayed queue and re-queued

#### Nack: Requeue After Delay

Return the task to the queue after a delay:

````go
return workerclient.Nack(60, "temporary outage, retrying in 60s")
````

Parameters:
- `delaySeconds`: Time to wait before task becomes claimable again
- `reason`: Human-readable reason for observability

Behavior:
- Task moved to delayed queue and becomes ready after delaySeconds
- Increment attempt counter
- Does not respect MaxAttempts (always re-queued)

#### Abandon: Release Without Nacking

Release the lease and return task to pending immediately:

````go
return workerclient.Abandon()
````

Use when worker is shutting down mid-task:
- Task goes straight back to pending (not delayed)
- Another worker can claim immediately
- Attempt counter unchanged
- Useful for graceful shutdown

### Concurrency Configuration

Control parallelism with `Concurrency`:

````go
// Sequential processing (default)
client, _ := workerclient.New(workerclient.Config{
    // Concurrency omitted or set to 0/1 → only 1 task at a time
    Addr:  "localhost:9091",
    Token: "token",
})

// Parallel processing
client, _ := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",
    Token:       "token",
    Concurrency: 50,  // Process up to 50 tasks in parallel
})
````

Each slot:
- Independently sends `Ready`
- Receives a `Task` (or waits with backoff if none available)
- Calls handler with task
- Sends `Result`
- Repeats

Slots run independently; one slot's failure doesn't block others.

### Command Filtering

Filter which commands this worker processes:

````go
client, _ := workerclient.New(workerclient.Config{
    Addr:     "localhost:9091",
    Token:    "token",
    Commands: []string{"GENERATE_MASTER", "DEPLOY_SERVICE"},
})
````

If `Commands` is empty or nil, the server uses the token's event types (from auth system).

### Graceful Shutdown

Implement graceful shutdown by cancelling the context:

````go
ctx, cancel := context.WithCancel(context.Background())

go func() {
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    <-sigChan
    log.Println("Shutdown signal received, cancelling context...")
    cancel()
}()

err := client.Run(ctx, myHandler)
// client.Run returns when ctx is cancelled
````

On shutdown:
- `Run` stops accepting new tasks
- In-flight tasks continue and complete (handler gets context cancelled soon after)
- Handler can check `ctx.Done()` to gracefully wind down

### Error Handling

#### Connection Errors

If the connection fails, `client.Run` returns the error:

````go
err := client.Run(ctx, myHandler)
if err != nil {
    log.Printf("Connection failed: %v", err)
    // Reconnect by calling client.Run again (or creating new client)
}
````

Reconnection:

````go
for {
    err := client.Run(ctx, myHandler)
    if err != nil {
        log.Printf("Connection error: %v, reconnecting...", err)
        time.Sleep(5 * time.Second)
        continue
    }
    break
}
````

#### Handler Panics

If `myHandler` panics, the slot catches and logs it. The panic does not crash the worker:

````go
// This panic is caught and logged; other slots keep running
func badHandler(ctx context.Context, task workerclient.Task) workerclient.Result {
    panic("oops")
}

client.Run(ctx, badHandler)  // Logs panic, keeps running
````

### TLS / mTLS Configuration

Same approach as producer client:

````go
import "google.golang.org/grpc/credentials"

tlsCreds, _ := credentials.NewClientTLSFromFile(
    "/path/to/ca.pem",
    "codeq.example.org",
)

client, _ := workerclient.New(workerclient.Config{
    Addr:   "codeq.example.org:9091",
    Token:  "token",
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(tlsCreds),
    },
})
````

### Logging and Observability

Pass a structured logger:

````go
import "log/slog"

logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

client, _ := workerclient.New(workerclient.Config{
    Addr:   "localhost:9091",
    Token:  "token",
    Logger: logger,
})
````

---

## Complete Example: Producer and Worker

### Producer Example

````go
package main

import (
    "context"
    "fmt"
    "log"
    
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
    // Connect to producer stream server
    client, err := producerclient.New(producerclient.Config{
        Addr:  "localhost:9092",
        Token: "producer-token",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    ctx := context.Background()
    session, err := client.Connect(ctx)
    if err != nil {
        log.Fatal(err)
    }
    defer session.Close()

    // Produce 1000 tasks
    for i := 0; i < 1000; i++ {
        taskID, err := session.Produce(ctx, producerclient.CreateRequest{
            Command:   "PROCESS_JOB",
            Payload:   []byte(fmt.Sprintf(`{"id":%d}`, i)),
            Priority:  i % 10,
        })
        if err != nil {
            log.Printf("Failed: %v", err)
        } else {
            fmt.Printf("Created: %s\n", taskID)
        }
    }
}
````

### Worker Example

````go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    
    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
    client, err := workerclient.New(workerclient.Config{
        Addr:        "localhost:9091",
        Token:       "worker-token",
        Concurrency: 10,
        Commands:    []string{"PROCESS_JOB"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    ctx := context.Background()
    err = client.Run(ctx, func(ctx context.Context, task workerclient.Task) workerclient.Result {
        var payload map[string]int
        json.Unmarshal(task.Payload, &payload)

        fmt.Printf("Processing task %s: %v\n", task.ID, payload)
        // Do work...

        return workerclient.Completed(map[string]any{
            "processed": true,
            "id":        payload["id"],
        })
    })
    if err != nil {
        log.Fatal(err)
    }
}
````

---

## Performance Characteristics

### Producer Streaming Benefits

**vs REST `/tasks` endpoint**:
- Eliminates per-call auth overhead (amortized to stream open)
- Enables async pipelining (many in-flight before first ack)
- Reduces TCP handshake + TLS overhead per call
- Typical improvement: **40-50% latency reduction, 2-3x throughput** for batch operations

**Throughput**:
- HTTP REST: ~33k creates/sec (per-call middleware ceiling)
- gRPC Streaming: 33k+ creates/sec per connection with pipelining
- Scales with Concurrency config

### Worker Streaming Benefits

**vs REST `/tasks/claim`, `/tasks/:id/result` calls**:
- Single stream for all claim-result cycles
- Amortized auth to stream open
- Reduced round-trip overhead
- Better scaling with Concurrency

**Latency**:
- Typical claim→result: 50-100ms latency reduction vs REST

---

## Protocol Buffer Reference

### Producer Protocol

**Client sends** `ProducerEvent`:
- `Hello {token}`: Auth message (sent once at stream open)
- `CreateTask {seq, command, payload, ...}`: Task submission (seq must be strictly monotonically increasing)

**Server sends** `ServerEvent`:
- `HelloAck {tenant_id, subject}`: Auth response
- `CreateAck {seq, ok, task_id, error_message}`: Task ack (echoes seq from CreateTask)
- `ServerError {code, message}`: Stream-level error

### Worker Protocol

See `internal/worker/proto/workerpb.proto` for worker protocol details.

---

## Troubleshooting

### Connection Refused

````
producerclient: dial localhost:9092: connection refused
````

**Cause**: Producer stream server not listening on that address
**Solution**: Check codeQ config, ensure `PRODUCER_STREAM_ADDR` is set and server is running

### Authentication Failed

````
producerclient: server rejected hello: Invalid token (ERR_INVALID_TOKEN)
````

**Cause**: Bearer token invalid or expired
**Solution**: Verify token format and expiry

### Stream Closed Unexpectedly

If `Produce` returns `stream closed: EOF`:
- Stream was closed by server (likely auth or protocol error)
- All pending produces unblock with error
- Create new Session to reconnect

### Slow Throughput

If throughput is lower than expected:
1. Check Concurrency config (if too low, limited parallelism)
2. Monitor latency of individual `Produce` calls
3. Verify network latency to server
4. Check server logs for errors or backpressure

---

## Migration from REST API

### Producer: REST → Streaming

Before (REST):
````go
for i := 0; i < 1000; i++ {
    resp, _ := http.Post(
        "http://localhost:8080/v1/codeq/tasks",
        "application/json",
        bytes.NewReader(taskJSON),
    )
    // Parse response...
}
````

After (Streaming):
````go
client, _ := producerclient.New(cfg)
session, _ := client.Connect(ctx)
for i := 0; i < 1000; i++ {
    taskID, _ := session.Produce(ctx, req)
}
````

Benefits:
- Single auth (vs 1000 auth checks)
- Async pipelining (concurrent requests in-flight)
- 2-3x faster

### Worker: REST → Streaming

Before (REST):
````go
for {
    resp, _ := http.Post("http://localhost:8080/v1/codeq/tasks/claim", ...)
    task := parseTask(resp)
    result := handler(task)
    http.Post("http://localhost:8080/v1/codeq/tasks/"+task.ID+"/result", ...)
}
````

After (Streaming):
````go
client, _ := workerclient.New(cfg)
client.Run(ctx, handler)
````

Benefits:
- Simpler code
- Automatic claim-result loop
- Single auth
- Better error handling

---

## See Also

- `docs/18-package-reference.md` - Package documentation for `pkg/producerclient` and `pkg/workerclient`
- `docs/04-http-api.md` - HTTP REST API reference (still available)
- `docs/17-performance-tuning.md` - Performance optimization guide
- `docs/09-security.md` - Authentication and security
- `docs/13-examples.md` - Additional code examples
