# Streaming APIs

This guide covers codeQ's high-throughput gRPC streaming APIs for producers and workers. Streaming APIs achieve **2-3x higher throughput** compared to REST by amortizing authentication overhead and enabling request pipelining.

## Overview

codeQ provides two complementary streaming APIs:

- **Producer Streaming**: Submit tasks via long-lived bidirectional gRPC stream
  - Pipelining: multiple `CreateTask` messages in-flight before acks arrive
  - Typical throughput: **33,000+ tasks/sec** per stream (vs. ~10,000 with REST)
  - Single authentication at stream open; tenant resolution once

- **Worker Streaming**: Claim and complete tasks via long-lived bidirectional gRPC stream
  - Concurrent slots: configurable parallelism with independent Ready-Task-Result cycles
  - Batching support: claim up to N tasks per cycle and submit results in batches
  - Typical throughput: **10x improvement** with batching enabled
  - Single authentication and tenant resolution per stream

Both APIs use the same token-based authentication as REST (bearer tokens, JWT validation, tenant isolation, event-type scopes).

## Configuration

### Server-Side Configuration

Enable streaming APIs in `codeq.yml`:

```yaml
# Producer gRPC streaming server
producerStreamAddr: :9092
# Shared auth, tenant resolution, rate limiting

# Worker gRPC streaming server
workerStreamAddr: :9091
# Shared auth, tenant resolution, rate limiting
```

Or via environment variables:

```bash
PRODUCER_STREAM_ADDR=:9092
WORKER_STREAM_ADDR=:9091
```

Both are optional; REST endpoints remain available regardless. Use the same bearer tokens and JWT claims as the HTTP API.

### Client-Side Configuration

#### Producer Streaming

```go
import "github.com/osvaldoandrade/codeq/pkg/producerclient"

cfg := producerclient.Config{
    Addr: "localhost:9092",
    Token: "bearer-token",
    // DialOptions for TLS/mTLS (optional)
}
client, err := producerclient.New(cfg)
defer client.Close()
```

#### Worker Streaming

```go
import "github.com/osvaldoandrade/codeq/pkg/workerclient"

cfg := workerclient.Config{
    Addr: "localhost:9091",
    Token: "bearer-token",
    Commands: []string{"process-image", "send-email"},
    Concurrency: 4,       // 4 parallel slots
    BatchSize: 10,        // batch 10 tasks per cycle (Phase 6 feature)
    // DialOptions for TLS/mTLS (optional)
}
client, err := workerclient.New(cfg)
defer client.Close()
```

## Producer Streaming API

### Protocol Flow

1. **Open stream** and send `Hello{token}`
2. **Authenticate** — server validates token, resolves tenant, replies with `HelloAck{tenant_id}`
3. **Send CreateTask** messages with monotonically-increasing `seq` numbers
4. **Pipeline freely** — multiple CreateTasks in-flight before any ack arrives
5. **Correlate acks** — server echoes `seq` in each `CreateAck{task_id}`, matching it to your request

### Tutorial: Producer Streaming

```go
package main

import (
    "context"
    "log"
    
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
    cfg := producerclient.Config{
        Addr: "codeq-server:9092",
        Token: "my-bearer-token",
    }
    
    client, err := producerclient.New(cfg)
    if err != nil {
        log.Fatalf("dial: %v", err)
    }
    defer client.Close()
    
    ctx := context.Background()
    session, err := client.Connect(ctx)
    if err != nil {
        log.Fatalf("connect: %v", err)
    }
    defer session.Close()
    
    // Submit a task; blocks until CreateAck arrives
    taskID, err := session.Produce(ctx, &producerclient.CreateRequest{
        Command: "process-file",
        Payload: []byte(`{"filename":"data.csv"}`),
        Priority: 5,
    })
    if err != nil {
        log.Fatalf("produce: %v", err)
    }
    log.Printf("task created: %s", taskID)
}
```

### Pipelining Example

```go
// Fire off 100 tasks and collect results as they arrive (not round-trip)
var results []string
for i := 0; i < 100; i++ {
    go func(idx int) {
        taskID, err := session.Produce(ctx, &producerclient.CreateRequest{
            Command: "batch-job",
            Payload: []byte(fmt.Sprintf(`{"job":%d}`, idx)),
        })
        if err != nil {
            log.Printf("produce [%d]: %v", idx, err)
            return
        }
        results = append(results, taskID)
    }(i)
}
// All tasks dispatched and acknowledged before any goroutine returns
```

### Error Handling

`session.Produce()` returns errors for:

- **Validation errors**: invalid command, malformed payload → retryable with corrected request
- **Rate limit**: token bucket exhausted → implement exponential backoff
- **Network errors**: connection dropped → reconnect and retry
- **Idempotency conflict**: same idempotency key submitted earlier → returned task ID matches previous submission

```go
taskID, err := session.Produce(ctx, &producerclient.CreateRequest{
    Command: "process",
    Payload: payload,
    IdempotencyKey: "unique-key-123",
})

var idempErr *producerclient.IdempotencyError
if errors.As(err, &idempErr) {
    // Same key submitted earlier; use idempErr.ExistingTaskID
    log.Printf("reused existing task: %s", idempErr.ExistingTaskID)
}
```

### Performance Notes

- **Throughput**: 33,000+ tasks/sec per stream (vs. ~10,000 with REST POST)
- **Latency**: similar per-request latency (e.g., 5-10ms), but parallelism hides it
- **Concurrency**: safe for concurrent `Produce()` calls from multiple goroutines (built-in queueing)
- **Batching** (Phase 6): combine multiple CreateTask into one `CreateTaskBatch` message for additional gRPC framing savings

### TLS/mTLS Configuration

```go
import "crypto/tls"
import "google.golang.org/grpc/credentials"

tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{clientCert},
    ServerName: "codeq-server",
    RootCAs: caCertPool,
}

cfg := producerclient.Config{
    Addr: "codeq-server:9092",
    Token: token,
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    },
}
```

## Worker Streaming API

### Protocol Flow

1. **Open stream** and send `Hello{token, worker_id}`
2. **Authenticate** — server validates token, resolves tenant + scopes, replies with `HelloAck{tenant_id}`
3. **Send Ready{count, commands}** — indicate capacity for up to `count` tasks with these commands
4. **Receive TaskAssignment** (single-task mode) or **TaskBatch** (batch mode, count > 1)
5. **Process task** in Handler callback
6. **Send Result** (single) or **ResultBatch** (batch mode)
7. **Repeat Ready** for next batch of work

Concurrency: spawn N independent slots (where N = `Config.Concurrency`), each running its own Ready-Task-Result cycle in parallel.

### Tutorial: Worker Streaming

```go
package main

import (
    "context"
    "log"
    
    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func handler(ctx context.Context, task workerclient.Task) workerclient.Result {
    log.Printf("processing task %s: %s", task.ID, task.Command)
    
    // Process the task
    result := processTask(task)
    
    // Return result (Completed, Failed, Nack, or Abandon)
    if result.IsSuccess {
        return workerclient.Completed([]byte(result.Output))
    }
    return workerclient.Failed(result.Error)
}

func main() {
    cfg := workerclient.Config{
        Addr: "codeq-server:9091",
        Token: "my-bearer-token",
        WorkerID: "worker-1",
        Commands: []string{"process-file", "send-email"},
        Concurrency: 4, // 4 parallel task slots
    }
    
    client, err := workerclient.New(cfg)
    if err != nil {
        log.Fatalf("dial: %v", err)
    }
    defer client.Close()
    
    ctx := context.Background()
    err = client.Run(ctx, handler)
    if err != nil {
        log.Fatalf("run: %v", err)
    }
}
```

### Batch Mode (Phase 6 / Q2 Feature)

Enable batching by setting `BatchSize > 1`:

```go
cfg := workerclient.Config{
    Addr: "codeq-server:9091",
    Token: token,
    Concurrency: 4,
    BatchSize: 10,  // claim up to 10 tasks per Ready
}
```

With `BatchSize` enabled:

- Each Ready claims up to 10 tasks (1 gRPC message)
- Handler is called once per task (still concurrent within the batch)
- Results are coalesced into a single ResultBatch (1 gRPC message)
- Pebble commit cost amortized across 10 tasks

Performance improvement: **10x better throughput** for batch sizes 10+.

### Concurrency and Slots

Each slot runs independently in a separate goroutine:

```
Worker Client (Concurrency=4)
├─ Slot 1: Ready → Task → Result → Ready → ...
├─ Slot 2: Ready → Task → Result → Ready → ...
├─ Slot 3: Ready → Task → Result → Ready → ...
└─ Slot 4: Ready → Task → Result → Ready → ...

Handler is invoked concurrently (up to 4 calls in parallel)
```

Failures in one slot don't block others. Each slot retries independently on network errors.

### Result Types

Return one of:

```go
// Task completed successfully with optional output
workerclient.Completed([]byte("result data"))

// Task failed permanently (respects MaxAttempts)
workerclient.Failed(errors.New("database connection failed"))

// Task should be requeued (soft error, respects backoff)
workerclient.Nack()

// Release the lease immediately; task returns to pending
// without decrementing MaxAttempts
workerclient.Abandon()
```

### Heartbeat (Lease Extension)

Streaming workers don't need explicit heartbeat — the worker client automatically extends leases on interval:

```go
cfg := workerclient.Config{
    Addr: "codeq-server:9091",
    Token: token,
    LeaseSeconds: 300,  // 5-minute lease
}
// Client automatically heartbeats at 50% of lease duration
// No explicit API call needed
```

For custom heartbeat logic, close and reconnect the stream.

### Error Handling

The Handler's `Result` communicates errors:

```go
func handler(ctx context.Context, task workerclient.Task) workerclient.Result {
    result, err := process(task)
    if err != nil {
        // Permanent error → Failed (respects MaxAttempts)
        if isFatal(err) {
            return workerclient.Failed(err)
        }
        // Transient error → Nack (will retry with backoff)
        return workerclient.Nack()
    }
    return workerclient.Completed(result)
}
```

Stream-level errors (auth, connection):

```go
err := client.Run(ctx, handler)
if err != nil {
    // Context cancelled, connection dropped, auth failure, etc.
    log.Fatalf("stream error: %v", err)
}
```

### TLS/mTLS Configuration

```go
import "crypto/tls"
import "google.golang.org/grpc/credentials"

tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{clientCert},
    ServerName: "codeq-server",
    RootCAs: caCertPool,
}

cfg := workerclient.Config{
    Addr: "codeq-server:9091",
    Token: token,
    DialOptions: []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    },
}
```

## Performance Characteristics

### Producer Streaming

| Metric | Value |
|--------|-------|
| Typical throughput | 33,000 tasks/sec per stream |
| Throughput vs REST | 2-3x improvement |
| Latency per task | ~5-10ms (same as REST) |
| Concurrency model | Pipelined async requests |
| Bottleneck | gRPC frame serialization; mitigated with batching |

### Worker Streaming

| Scenario | Throughput | Notes |
|----------|-----------|-------|
| Single-task mode (BatchSize=1) | ~5,000-10,000 tasks/sec | Similar to REST |
| Batch mode (BatchSize=10) | ~50,000-100,000 tasks/sec | 10x improvement |
| Batch mode (BatchSize=50) | ~150,000+ tasks/sec | Optimal for batch workloads |

Concurrency: each additional slot (Concurrency=N) roughly adds N× throughput up to saturation.

## Protocol Reference

### Producer Protocol Messages

**Hello** (producer → server, first message)
```
token: string
```

**HelloAck** (server → producer)
```
tenant_id: string
subject: string
```

**CreateTask** (producer → server, repeating)
```
seq: uint64              // monotonically increasing per stream
command: string
payload: bytes           // JSON
priority: int32
webhook: string
max_attempts: int32
idempotency_key: string
run_at: Timestamp        // optional
delay_seconds: int32     // optional
trace_parent: string     // W3C trace context
```

**CreateAck** (server → producer)
```
seq: uint64              // echoes producer's seq
task_id: string          // assigned task ID, or error_message if failed
error_message: string
```

### Worker Protocol Messages

**Hello** (worker → server, first message)
```
token: string
worker_id: string        // optional; server uses JWT subject if empty
```

**HelloAck** (server → worker)
```
tenant_id: string
subject: string
```

**Ready** (worker → server, repeating)
```
commands: string[]       // empty = any command
count: int32             // 0/1 = single-task mode, >1 = batch mode
lease_seconds: int32     // 0 = server default
```

**TaskAssignment** (server → worker, single-task mode)
```
Task {
  id: string
  command: string
  payload: bytes         // JSON
  priority: int32
  webhook: string
  max_attempts: int32
  status: string
  worker_id: string
  lease_until: string    // RFC3339
  attempts: int32
  tenant_id: string
  created_at: Timestamp
  updated_at: Timestamp
}
```

**TaskBatch** (server → worker, batch mode, count > 1)
```
tasks: Task[]            // up to count tasks
```

**Result** (worker → server)
```
task_id: string
status: string          // "Completed", "Failed", "Nack", "Abandon"
output: bytes           // optional, for Completed
error: string           // optional, for Failed
lease_until: string     // for Heartbeat
```

**ResultBatch** (worker → server, batch mode)
```
results: Result[]
```

## Comparison: REST vs Streaming

| Aspect | REST | Streaming |
|--------|------|-----------|
| **Per-request auth** | Yes (expensive) | No (once at open) |
| **Per-request tenant resolution** | Yes | No (once at open) |
| **HTTP middleware overhead** | Per-call | Amortized |
| **Request pipelining** | Hard (HTTP/1.1 sequential) | Native (gRPC streaming) |
| **Throughput** | 10,000 tasks/sec | 33,000+ tasks/sec (producer) |
| **Latency** | ~5-10ms | ~5-10ms (same, pipelining hides it) |
| **Connection reuse** | Limited | Yes, single stream |
| **Batch support** | No | Yes (Phase 6) |
| **Ideal for** | Low-throughput, simple scripts | High-throughput, always-on workers |

## Migration Guide

### From REST Producer to Streaming

**Before (REST):**
```go
for i := 0; i < 1000; i++ {
    resp, err := http.Post("http://server/v1/codeq/tasks", "application/json", ...)
    // ~10 tasks/sec
}
```

**After (Streaming):**
```go
session, _ := client.Connect(ctx)
for i := 0; i < 1000; i++ {
    _, err := session.Produce(ctx, req)
    // Can pipeline; ~33k tasks/sec effective
}
```

### From REST Worker to Streaming

**Before (REST):**
```go
for {
    task, _ := http.Post("http://server/v1/codeq/claim", ...)
    result := process(task)
    http.Post(fmt.Sprintf("http://server/v1/codeq/tasks/%s/result", task.ID), ...)
}
```

**After (Streaming):**
```go
client, _ := workerclient.New(workerclient.Config{
    Addr: "server:9091",
    Token: token,
    Concurrency: 4,
    BatchSize: 10,
})
client.Run(ctx, handler)
```

## Troubleshooting

### Connection Refused

- Ensure `producerStreamAddr` / `workerStreamAddr` are set in server config
- Check firewall rules for the streaming port
- Verify server is running: `netstat -tlnp | grep 9091`

### Authentication Failed

- Verify bearer token is valid (same format as REST)
- Check token claims: must include `iat`, `exp`, and appropriate event-type scopes
- Ensure token hasn't expired

### High Latency or Stalls

- **Producer**: increase pipeline depth (more concurrent Produce calls)
- **Worker**: increase `Concurrency` and/or `BatchSize`
- Check server CPU/memory: streaming concentrates load per connection
- Review Prometheus metrics: `codeq_worker_stream_tasks_total`, `codeq_producer_stream_tasks_total`

### Memory Growth

- Worker streaming holds active leases in memory (Phase 6 optimization)
- Each active task ~32 bytes; 1M concurrent tasks ≈ 32 MiB
- Monitor `process_resident_memory_bytes` and task count

### Dropped Connections

- Network timeouts: check firewall keep-alive rules
- Server overload: reduce `Concurrency` or implement backoff
- Reconnect logic: implement exponential backoff before retry

## See Also

- `docs/04-http-api.md` — REST API reference
- `docs/14-configuration.md` — Full server configuration
- `docs/17-performance-tuning.md` — Performance optimization guide
- `docs/03-architecture.md` — System architecture
- `internal/producer/producerpb.proto` — Producer protocol definition
- `internal/worker/workerpb.proto` — Worker protocol definition
