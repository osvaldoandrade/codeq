# Streaming APIs (gRPC)

codeQ provides high-throughput gRPC streaming APIs for producers and workers, achieving 2-3× the throughput of REST by amortizing authentication and enabling pipelined request processing.

## Overview

### Why streaming?

The REST API's per-call overhead—authentication validation, tenant resolution, route matching, middleware chains—dominates latency under high load. The streaming APIs eliminate this by:

1. **Single authentication** at stream-open time; subsequent calls reuse the authenticated context
2. **Pipelined requests** allowing many in-flight operations without blocking
3. **Batched acknowledgments** combining multiple task results into one round-trip

Benchmarks show:
- **Producer streaming**: ~33k tasks/sec per stream (vs ~15k with REST)
- **Worker streaming**: 2-3× throughput improvement with batching enabled

### Design principles

- **Backward compatible**: REST API unchanged; streaming is opt-in
- **Protocol-compatible**: gRPC messages mirror REST request/response bodies
- **Failure-isolated**: One worker's error doesn't block peer connections
- **Observable**: Structured logging, metrics, and trace context propagation

---

## Producer Streaming API

Producers submit tasks at high throughput by pipelining `CreateTask` events over a single long-lived gRPC stream.

### Quick start (Go)

````go
import "github.com/osvaldoandrade/codeq/pkg/producerclient"

// Connect once, reuse for many tasks
client, err := producerclient.New(producerclient.Config{
    Addr:  "localhost:9092",
    Token: "your-producer-token",
})
defer client.Close()

// Open a session (can open many per Client)
sess, err := client.Connect(ctx)
defer sess.Close()

// Produce tasks concurrently from goroutines
taskID, err := sess.Produce(ctx, producerclient.CreateRequest{
    Command: "handle-payment",
    Payload: []byte(`{"user_id":"123","amount":99.99}`),
    Priority: 10,
    Webhook: "https://myapp.example.com/webhook",
})
// Returns immediately after server acks; many Produce calls can be in flight
````

### Protocol flow

```
Producer                         Server
   |
   +-- Hello(token) ------------>|  Authenticate, resolve tenant
   |<--------- HelloAck ----------+  Return tenant_id, subject
   |
   +-- CreateTask(seq=1) ------->|
   +-- CreateTask(seq=2) ------->|  Pipelined: producer does not wait
   +-- CreateTask(seq=3) ------->|
   |
   |<------- CreateAck(seq=1) ----+  Task accepted; task_id assigned
   |<------- CreateAck(seq=2) ----+  Acks arrive in any order
   |<------- CreateAck(seq=3) ----+
```

### API Reference

#### producerclient.Config

```go
type Config struct {
    // Addr: gRPC dial target (e.g., "localhost:9092"). Required.
    Addr string

    // Token: Bearer token for authentication. Required.
    Token string

    // DialOptions: forwarded to grpc.NewClient. If empty, uses insecure
    // transport. Set this for TLS/mTLS.
    DialOptions []grpc.DialOption

    // Logger: slog.Logger for info/warn/error events. Defaults to slog.Default().
    Logger *slog.Logger
}
```

#### producerclient.New

Opens a gRPC connection to the server. The returned Client can create multiple Sessions over its lifetime.

```go
client, err := producerclient.New(cfg)
// Connection stays open until Close()
defer client.Close()
```

#### Client.Connect

Opens a bidirectional stream, completes the Hello handshake, and returns a Session ready for Produce calls.

```go
sess, err := client.Connect(ctx)
// Session is tied to ctx; canceling ctx cancels the session
```

#### Session.Produce

Sends one CreateTask and blocks until the server acknowledges. Safe for concurrent calls from many goroutines (naturally pipelines across callers).

```go
taskID, err := sess.Produce(ctx, producerclient.CreateRequest{
    Command:        "handle-order",          // Required
    Payload:        []byte{...},             // Optional
    Priority:       5,                       // Optional; default 0
    Webhook:        "https://...",           // Optional
    MaxAttempts:    3,                       // Optional; default per config
    IdempotencyKey: "user:123:order:456",   // Optional; deduplicate by key
    RunAt:          time.Now().Add(1*time.Hour), // Optional; schedule delay
    DelaySeconds:   0,                       // Optional; seconds until eligible
    TraceParent:    "00-...",                // Optional; W3C trace header
    TraceState:     "...",                   // Optional; W3C trace state
})
```

Returns the assigned task ID, or an error if the task was rejected.

**Error cases:**
- `"producerclient: session closed"` — Session was closed or ctx cancelled
- `"producerclient: stream closed"` — Underlying gRPC stream died
- Task-level errors from validation or deduplication (e.g., idempotency conflict)

#### Session.Close

Releases the reader goroutine and closes the underlying stream.

```go
sess.Close()
```

### Error handling

**Transient errors** (network flakes, temporary service issues):

```go
for attempt := 0; attempt < maxRetries; attempt++ {
    taskID, err := sess.Produce(ctx, req)
    if err == nil {
        return taskID, nil
    }
    if !isRetryable(err) {
        return "", err
    }
    select {
    case <-time.After(backoff(attempt)):
    case <-ctx.Done():
        return "", ctx.Err()
    }
}
```

**Permanent errors** (validation, auth, duplicate key):
- Log and move on; don't retry the same request

**Stream death** (context cancelled, server shutdown):
- Close the session and either reconnect or propagate error to caller
- Pending in-flight requests are fanned out the stream error; `Produce` returns the same error

### Performance tuning

**Pipeline depth**: How many `Produce` calls should be in flight at once? Start with 10–100 and measure:

```go
// Fire off 50 Produce calls without waiting for acks
var taskIDs []string
for i := 0; i < 50; i++ {
    go func() {
        taskID, _ := sess.Produce(ctx, req)
        taskIDs = append(taskIDs, taskID)
    }()
}
// Let server amortize the work
```

**Batch size**: One session can produce 33k tasks/sec; use multiple sessions if you need higher throughput from a single process.

### TLS/mTLS

Pass `grpc.WithTransportCredentials` in Config.DialOptions:

```go
creds, _ := credentials.NewClientTLSFromFile("ca.pem", "")
client, _ := producerclient.New(producerclient.Config{
    Addr: "localhost:9092",
    Token: token,
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(creds),
    },
})
```

---

## Worker Streaming API

Workers claim and submit tasks over a persistent gRPC stream, with support for concurrent task processing via configurable concurrency slots.

### Quick start (Go)

````go
import "github.com/osvaldoandrade/codeq/pkg/workerclient"

client, err := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",
    Token:       "your-worker-token",
    Concurrency: 8,        // Run 8 tasks in parallel
    BatchSize:   10,       // Claim up to 10 tasks per Ready
})
defer client.Close()

// Run blocks until ctx cancelled or stream error
err = client.Run(ctx, func(ctx context.Context, task workerclient.Task) workerclient.Result {
    // Handler is invoked concurrently for each claimed task
    result, err := myApp.HandleTask(task.Command, task.Payload)
    if err != nil {
        return workerclient.Failed(err.Error())
    }
    return workerclient.Completed(map[string]any{"result": result})
})
````

### Protocol flow

```
Worker                           Server
   |
   +-- Hello(token, worker_id) -->|  Authenticate; resolve scopes
   |<--------- HelloAck -----------+  Confirm tenant_id, worker_id
   |
   +-- Ready(commands=[...], count=1) -->|  Claim one task
   |<--------- TaskAssignment -----+  Task details
   |
   +-- Result(task_id, status=COMPLETED) -->|
   |<--------- ResultAck -----------+  Ack (or error if task not found)
   |
   [repeat: slot loops independently]
```

With batching (BatchSize > 1):

```
Worker                           Server
   +-- Ready(commands=[...], count=10) -->|  Claim up to 10 tasks
   |<--------- TaskBatch(tasks=[...]) ----+  Server batches tasks together
   |
   +-- ResultBatch(results=[...]) ------->|  Submit all in one message
   |<--------- ResultAckBatch ------+  Batch of acks
```

### API Reference

#### workerclient.Config

```go
type Config struct {
    // Addr: gRPC dial target (e.g., "localhost:9091"). Required.
    Addr string

    // Token: Bearer token for authentication. Required.
    Token string

    // WorkerID: Identifies this worker for lease ownership. If empty, the
    // server uses the JWT subject from Token.
    WorkerID string

    // Commands: Restricts what this worker pulls (filter on task.Command).
    // nil/empty means "allow all commands permitted by token claims".
    Commands []string

    // Concurrency: Number of in-flight tasks. Defaults to 1.
    // Each slot independently: sends Ready → receives Task(s) → calls Handler.
    // Set to 8–16 for most workloads.
    Concurrency int

    // LeaseSeconds: Sent on each Ready. 0 means server default.
    LeaseSeconds int

    // BatchSize: How many tasks per Ready, and how many Results per
    // ResultBatch. 0/1 = legacy single-task path (one Task per Ready).
    // >1 = Phase 6 batching (TaskBatch, ResultBatch). Default 1.
    BatchSize int

    // IdleBackoff: How long a slot waits before re-sending Ready when the
    // previous Ready didn't yield a task. Defaults to 50ms.
    IdleBackoff time.Duration

    // DialOptions: forwarded to grpc.NewClient. If empty, uses insecure.
    DialOptions []grpc.DialOption

    // Logger: slog.Logger for info/warn/error events.
    Logger *slog.Logger
}
```

#### workerclient.Handler

A function that processes one task and returns its disposition.

```go
type Handler func(ctx context.Context, t Task) Result
```

The handler runs concurrently (up to Config.Concurrency calls in parallel). Each call receives a unique Task; context is cancelled if the stream dies or the worker shuts down.

#### workerclient.Task

```go
type Task struct {
    ID          string   // Task ID (unique per tenant)
    Command     string   // The command label
    Payload     []byte   // Raw bytes (usually JSON)
    Priority    int      // 0–2^31-1; higher wins
    Attempts    int      // Number of claim attempts so far
    MaxAttempts int      // Configured max; FAILED if exceeded
    TenantID    string   // Multi-tenant isolation
    Webhook     string   // Optional callback URL
    LeaseUntil  string   // RFC3339 lease expiration time
}
```

#### workerclient.Result

```go
// Completed marks a task done with optional result payload
func Completed(body map[string]any) Result

// Failed marks a task permanently failed (respects MaxAttempts)
func Failed(err string) Result

// Nack returns the task to the queue after delaySeconds
func Nack(delaySeconds int, reason string) Result

// Abandon releases the lease without nacking (for graceful shutdown)
func Abandon() Result
```

#### Client.Run

Opens a stream, authenticates, and dispatches claimed tasks to the handler until ctx is cancelled or a fatal stream error occurs.

```go
err := client.Run(ctx, handler)
```

### Result types

| Result | Behavior | When to use |
|--------|----------|------------|
| **Completed** | Mark task done; store result payload; trigger webhooks | Normal success path |
| **Failed** | Increment attempts; if attempts ≥ MaxAttempts, move to DLQ; otherwise requeue | Permanent errors (validation, unrecoverable logic error) |
| **Nack** | Return task to queue with optional delay; resume normal scheduling | Transient issues (rate limit hit, dependency down); does not count as attempt |
| **Abandon** | Release lease without nacking; task goes to Pending immediately | Worker shutting down mid-task |

### Concurrency model

Each of `Config.Concurrency` slots runs independently:

1. Send `Ready(commands, lease_seconds, count=BatchSize)`
2. Receive `TaskAssignment` (single task) or `TaskBatch` (N tasks)
3. Call `Handler` for each task (sequentially within a slot)
4. Send `Result` or `ResultBatch` with results
5. Loop back to step 1

**Key properties:**
- One slot's error doesn't block other slots
- Slots run in parallel; no global lock on result submission
- Handler calls are sequential within a slot but parallel across slots
- All slots are cancelled together when ctx.Done() fires

### Error handling

**Handler errors** (user code):

```go
func myHandler(ctx context.Context, t workerclient.Task) workerclient.Result {
    result, err := process(t)
    if err != nil {
        if isTransient(err) {
            return workerclient.Nack(5, fmt.Sprintf("transient: %v", err))
        }
        return workerclient.Failed(fmt.Sprintf("permanent: %v", err))
    }
    return workerclient.Completed(result)
}
```

**Stream errors** (infrastructure):

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

err := client.Run(ctx, handler)
if err == context.DeadlineExceeded {
    log.Printf("worker timeout")
} else if err != nil {
    log.Printf("stream died: %v", err)
}
```

**Graceful shutdown**:

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM)

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

go func() {
    <-sigCh
    cancel()  // Cancels all slots
}()

if err := client.Run(ctx, handler); err != nil && err != context.Canceled {
    log.Fatalf("worker died: %v", err)
}
```

### Performance tuning

**Concurrency**: Number of parallel task slots.

- **Low concurrency** (1–2): Minimal memory; suitable for CPU-bound handlers or IO-limited queues
- **Medium concurrency** (8–16): Good for mixed workloads; amortizes server overhead
- **High concurrency** (32–128): For IO-bound handlers or very fast task processing

Start with 8 and measure throughput and latency.

**BatchSize**: How many tasks to claim per Ready.

- **BatchSize=1** (default): Legacy single-task path; lowest latency per task but higher per-task overhead
- **BatchSize=10–50**: Batch latency added but significant throughput improvement (2-3× with Phase 6 optimizations)

Use BatchSize > 1 only if:
1. Tasks are small (processing time < 100ms)
2. You need throughput over latency
3. Handler gracefully handles varying batch sizes

**Example: high-throughput worker**

```go
client, err := workerclient.New(workerclient.Config{
    Addr:        "localhost:9091",
    Token:       token,
    Concurrency: 32,    // Many parallel slots
    BatchSize:   50,    // Claim 50 at once
    Commands:    []string{"process-log-entry"},
})
```

### Lease management

Each Ready specifies `LeaseSeconds`; the server grants the worker exclusive claim on the task until `LeaseUntil` (RFC3339 timestamp in Task.LeaseUntil).

**Automatic renewal**: Handler context is tied to the lease. If handler takes longer than `LeaseSeconds`, the task may be reclaimed by another worker or moved to Pending.

**Heartbeat** (advanced, not yet exposed in Go SDK): Send a Heartbeat to extend the lease mid-task.

### Nack behavior

Nack returns a task with optional delay:

```go
workerclient.Nack(30, "rate_limit_hit")  // Requeue in 30 seconds
```

If the task was already in the DLQ (attempts ≥ MaxAttempts), it stays in the DLQ.

### TLS/mTLS

```go
creds, _ := credentials.NewClientTLSFromFile("ca.pem", "")
client, _ := workerclient.New(workerclient.Config{
    Addr: "localhost:9091",
    Token: token,
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(creds),
    },
})
```

---

## Configuration

### Server-side streaming endpoints

The codeQ server exposes two gRPC services:

| Service | Port | Purpose |
|---------|------|---------|
| **ProducerStream** | 9092 | Producer task submission |
| **WorkerStream** | 9091 | Worker task claiming & result submission |

Configure via environment or config file:

```yaml
# config.yaml
grpc:
  producer:
    addr: "0.0.0.0:9092"
  worker:
    addr: "0.0.0.0:9091"
```

Or:

```bash
export CODEQ_GRPC_PRODUCER_ADDR="0.0.0.0:9092"
export CODEQ_GRPC_WORKER_ADDR="0.0.0.0:9091"
```

### Authentication

Both APIs use bearer token authentication:

1. **Producer**: Token from `Hello.token`; typically a service-to-service credential
2. **Worker**: Token from `Hello.token`; typically a worker-scoped JWT with `codeq:claim`, `codeq:result` scopes

Tokens are validated by the pluggable auth system (default: JWKS). See [09-security.md](09-security.md) for details.

### Rate limiting

Optional per-token rate limiting applies to both APIs:

```yaml
ratelimit:
  enabled: true
  default_rate: "1000 req/sec"
  overrides:
    producer-token-1: "50000 req/sec"
    worker-token-1: "10000 req/sec"
```

---

## Metrics & Observability

### Prometheus metrics

Streaming API calls emit the same metrics as REST:

- `codeq_task_created_total` — Tasks created (gRPC or REST)
- `codeq_task_claimed_total` — Tasks claimed
- `codeq_task_completed_total` — Tasks completed
- `codeq_task_failed_total` — Tasks failed
- `codeq_task_nacked_total` — Tasks nacked

### Structured logging

The client libraries emit structured logs (info, warn, error) via slog:

```
time=2024-05-17T10:30:45.123Z level=INFO msg="producerclient: hello ok" tenantId=acme subject=app-backend
time=2024-05-17T10:30:46.456Z level=WARN msg="producerclient: ack for unknown seq" seq=999
```

### Distributed tracing

Enable OpenTelemetry tracing in the codeQ server:

```yaml
tracing:
  enabled: true
  otlp:
    grpc_endpoint: "localhost:4317"
```

The client libraries propagate W3C trace context (TraceParent, TraceState) automatically.

---

## Examples

### Producer: high-throughput task submission

```go
package main

import (
    "context"
    "log"
    "sync"
    "time"

    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

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

    // Produce 1000 tasks, pipelined
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    var wg sync.WaitGroup
    for i := 0; i < 1000; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            _, err := sess.Produce(ctx, producerclient.CreateRequest{
                Command: "process",
                Payload: []byte("{}"),
            })
            if err != nil {
                log.Printf("produce failed: %v", err)
            }
        }(i)
    }
    wg.Wait()
    log.Println("done")
}
```

### Worker: batched task processing

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
    client, err := workerclient.New(workerclient.Config{
        Addr:        "localhost:9091",
        Token:       "worker-token",
        Concurrency: 16,
        BatchSize:   20,
        Commands:    []string{"process-order", "send-email"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    ctx, cancel := context.WithCancel(context.Background())
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

    go func() {
        <-sigCh
        log.Println("shutting down...")
        cancel()
    }()

    err = client.Run(ctx, func(ctx context.Context, t workerclient.Task) workerclient.Result {
        log.Printf("processing task %s: %s", t.ID, t.Command)
        
        // Your business logic here
        result, err := processTask(ctx, t)
        if err != nil {
            if isRetryable(err) {
                return workerclient.Nack(5, err.Error())
            }
            return workerclient.Failed(err.Error())
        }
        return workerclient.Completed(result)
    })

    if err != nil && err != context.Canceled {
        log.Fatalf("worker died: %v", err)
    }
}

func processTask(ctx context.Context, t workerclient.Task) (map[string]any, error) {
    // Your implementation
    return map[string]any{"status": "ok"}, nil
}

func isRetryable(err error) bool {
    // Your logic
    return false
}
```

---

## Troubleshooting

### Connection refused

**Symptom**: `dial ... refused`

**Cause**: Server gRPC endpoint not listening

**Fix**:
1. Verify server is running: `ps aux | grep codeq`
2. Check port binding: `netstat -tlnp | grep 9091` (worker), `9092` (producer)
3. Check firewall: `iptables -L | grep 9091`

### Unauthenticated (code 16)

**Symptom**: `rpc error: code = Unauthenticated desc = ...`

**Cause**: Invalid or expired bearer token

**Fix**:
1. Verify token format: should be a valid JWT
2. Check token scopes: worker needs `codeq:claim`, `codeq:result`
3. Verify JWKS endpoint is reachable and up-to-date

### Stream closed unexpectedly

**Symptom**: `Produce/Run returns stream closed error`

**Cause**: Server closed stream (auth error, server shutdown, rate limit exceeded)

**Fix**:
1. Check server logs for errors
2. Verify rate limits are not exceeded
3. Check network connectivity (packet loss, firewall rules)
4. Implement client-side reconnection logic with exponential backoff

### Latency or throughput issues

**Symptom**: Produce/claim latency high or throughput low

**Diagnosis**:
1. Measure RTT: `ping -c 10 server`
2. Check server CPU: `top` on server
3. Check network bandwidth: `iftop`
4. Verify batch size is appropriate for your workload

**Tuning**:
- Increase producer pipeline depth (more concurrent `Produce` calls)
- Increase worker concurrency or batch size
- Enable server CPU profile: `go tool pprof http://server:6060/debug/pprof/profile`

---

## See also

- [04-http-api.md](04-http-api.md) — REST API reference (unchanged)
- [09-security.md](09-security.md) — Authentication & authorization
- [14-configuration.md](14-configuration.md) — Server configuration
- [17-performance-tuning.md](17-performance-tuning.md) — Performance optimization guide
