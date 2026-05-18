# Tutorial: Go SDK end-to-end

A complete walkthrough that builds a small Go service in two parts:

- a **producer** that enqueues tasks over the gRPC streaming SDK
  (`pkg/producerclient`);
- a **worker** that claims and processes them
  (`pkg/workerclient`).

By the end you have two binaries you can run against a local codeq
server. The code compiles unchanged against the API surface in
[`pkg/producerclient/client.go`](../pkg/producerclient/client.go) and
[`pkg/workerclient/client.go`](../pkg/workerclient/client.go).

## Table of contents

- [1. What you'll build](#1-what-youll-build)
- [2. Prerequisites](#2-prerequisites)
- [3. Project setup](#3-project-setup)
- [4. Producer](#4-producer)
- [5. Worker](#5-worker)
- [6. Result handling — when to use which](#6-result-handling--when-to-use-which)
- [7. Concurrency, batching, lease tuning](#7-concurrency-batching-lease-tuning)
- [8. Where the gRPC connections go](#8-where-the-grpc-connections-go)
- [9. Real-world patterns](#9-real-world-patterns)
- [10. Next steps](#10-next-steps)

## 1. What you'll build

A small Go service with two parts: a producer that creates tasks, and
a worker that processes them. Both use gRPC streaming clients
([`pkg/producerclient`](../pkg/producerclient/client.go) and
[`pkg/workerclient`](../pkg/workerclient/client.go)) that open one
bidirectional stream per process, authenticate once with a `Hello`
event, and then pipeline `CreateTask` / `Ready` / `Result` events over
the same TCP connection. This avoids the per-request middleware tax of
the REST endpoint — the harness measures the streaming full-cycle at
83,420 tasks/s on a 12-core box
(`internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle`,
see [`_STYLE.md` § 7](./_STYLE.md#7-numbers-must-come-from-measurement)).

## 2. Prerequisites

- Go 1.22 or newer (the repo currently builds on 1.25).
- A running codeq server. The fastest path is single-node Pebble per
  [Getting started](./00-getting-started.md). For a multi-node setup
  see [Raft replication](./40-raft-replication.md).
- A producer token and a worker token. With the default config the
  string `dev-token` works for both (see
  [`deploy/config/codeq.example.yml`](../deploy/config/codeq.example.yml)).
  In production, mint real tokens — see [Security](./09-security.md).
- The default ports: `:9092` for the producer stream and `:9091` for
  the worker stream. The HTTP API on `:8080` is unused in this
  tutorial.

## 3. Project setup

```bash
mkdir my-codeq-app && cd my-codeq-app
go mod init example/codeq-app
go get github.com/osvaldoandrade/codeq
mkdir -p producer worker
```

You should end up with the layout:

```text
my-codeq-app/
├── go.mod
├── go.sum
├── producer/
│   └── main.go
└── worker/
    └── main.go
```

## 4. Producer

The producer opens **one** `Client`, then **one** `Session` on that
client, and reuses both for the lifetime of the process. Each
`Session` is a single gRPC stream; many goroutines can call `Produce`
concurrently on it and the client multiplexes via sequence numbers
([`client.go:104-131`](../pkg/producerclient/client.go)).

`producer/main.go`:

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "time"

    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
    // Construct the client. Holds one gRPC connection; safe to share
    // across goroutines.
    cli, err := producerclient.New(producerclient.Config{
        Addr:  os.Getenv("CODEQ_PRODUCER_ADDR"), // e.g. "localhost:9092"
        Token: os.Getenv("CODEQ_PRODUCER_TOKEN"),
    })
    if err != nil {
        log.Fatalf("producerclient.New: %v", err)
    }
    defer cli.Close()

    // Open one bidirectional stream. Multiple goroutines can call
    // Produce concurrently on this Session — sends are funnelled
    // through an internal writer goroutine and acks are routed back
    // by sequence number.
    ctx := context.Background()
    sess, err := cli.Connect(ctx)
    if err != nil {
        log.Fatalf("Connect: %v", err)
    }
    defer sess.Close()

    // Marshal a JSON payload. codeq treats payloads as opaque bytes;
    // any encoding works as long as your worker agrees.
    payload, _ := json.Marshal(map[string]any{
        "orderId": "42",
        "amount":  99.99,
    })

    // Produce. Returns the server-assigned task id once the matching
    // CreateAck arrives. Blocks only on that single round trip.
    taskID, err := sess.Produce(ctx, producerclient.CreateRequest{
        Command:  "PROCESS_ORDER",
        Payload:  payload,
        Priority: 5,
        // Optional: IdempotencyKey: "order-42",
        // Optional: DelaySeconds:   60, // visible 60s in the future
    })
    if err != nil {
        log.Fatalf("Produce: %v", err)
    }
    log.Printf("created task %s", taskID)

    // Give the worker time to consume in this demo. In a real
    // service the producer keeps the Session alive for the lifetime
    // of the process.
    time.Sleep(2 * time.Second)
}
```

Run:

```bash
CODEQ_PRODUCER_ADDR=localhost:9092 CODEQ_PRODUCER_TOKEN=dev-token go run ./producer
```

If the producer needs to enqueue many tasks at once, prefer
`Session.ProduceBatch`. One stream frame carries N `CreateTask` and
the server replies with one `CreateAckBatch`. Per-task work still
parallelises on the server side
([`client.go:355-421`](../pkg/producerclient/client.go)).

## 5. Worker

The worker uses `workerclient.Client.Run`, which blocks until the
context is cancelled. Internally it opens a stream, runs the `Hello`
handshake, then spawns `Concurrency` slot goroutines. Each slot loops:
send `Ready` → receive `Task` (or `TaskBatch`) → invoke the handler →
send `Result` (or coalesced `ResultBatch`).

`worker/main.go`:

```go
package main

import (
    "context"
    "encoding/json"
    "errors"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
    w, err := workerclient.New(workerclient.Config{
        Addr:         os.Getenv("CODEQ_WORKER_ADDR"), // e.g. "localhost:9091"
        Token:        os.Getenv("CODEQ_WORKER_TOKEN"),
        Commands:     []string{"PROCESS_ORDER"},
        Concurrency:  4,   // 4 in-flight tasks per process
        LeaseSeconds: 120, // each task gets a 2-minute lease
        // BatchSize: 8,   // optional: claim 8 at a time + batch results
    })
    if err != nil {
        log.Fatalf("workerclient.New: %v", err)
    }
    defer w.Close()

    // Cancel cleanly on SIGINT/SIGTERM. Run returns when ctx is done.
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    handler := func(ctx context.Context, t workerclient.Task) workerclient.Result {
        var order struct {
            OrderID string  `json:"orderId"`
            Amount  float64 `json:"amount"`
        }
        if err := json.Unmarshal(t.Payload, &order); err != nil {
            // Bad payload — not retriable. Fail terminally; counts
            // toward MaxAttempts and (eventually) goes to the DLQ.
            return workerclient.Failed("invalid payload: " + err.Error())
        }

        log.Printf("processing %s (orderId=%s amount=%.2f)",
            t.ID, order.OrderID, order.Amount)

        if err := process(ctx, order.OrderID, order.Amount); err != nil {
            if errors.Is(err, context.Canceled) {
                // Graceful shutdown — release the lease so another
                // worker can pick up immediately (no attempt++).
                return workerclient.Abandon()
            }
            // Transient failure — retry after 5s. Attempts++.
            return workerclient.Nack(5, err.Error())
        }

        // Success. The result body is JSON-encoded and stored in
        // KeyResult for the producer to fetch via GetResult.
        return workerclient.Completed(map[string]any{
            "orderId":   order.OrderID,
            "processed": true,
        })
    }

    if err := w.Run(ctx, handler); err != nil {
        log.Printf("worker exited: %v", err)
    }
    log.Println("worker stopped")
}

func process(ctx context.Context, orderID string, amount float64) error {
    // Your real logic here. Honor ctx so Abandon works on shutdown.
    return nil
}
```

Run:

```bash
CODEQ_WORKER_ADDR=localhost:9091 CODEQ_WORKER_TOKEN=dev-token go run ./worker
```

The handler signature is `func(ctx, Task) Result`. It runs on a slot
goroutine inside the client; the client guarantees that the handler is
invoked concurrently up to `Concurrency` times, so any shared state
inside must be safe under that many goroutines.

## 6. Result handling — when to use which

The `Result` constructors live in
[`pkg/workerclient/result.go`](../pkg/workerclient/result.go). Pick
based on whether the failure is retriable and whether you want the
attempt counter to advance:

| Constructor | Semantics | Attempts++ | Use when |
|---|---|---|---|
| `Completed(body map[string]any)` | Terminal success. Body is JSON-encoded and stored in `KeyResult`. | n/a | Work is done. |
| `Failed(reason string)` | Terminal failure. Counts toward `MaxAttempts`; past the limit the task goes to the DLQ. | yes | Bad payload, validation error, or any error you know retrying won't fix. |
| `Nack(backoffSec int, reason string)` | Transient failure. Re-enqueued after `backoffSec`. | yes | Downstream 503, network blip, lock contention. |
| `Abandon()` | Release the lease without success or failure. Another worker can claim it immediately. | no | Graceful shutdown mid-task — the next worker retries with the original attempt count. |

The signature of `Completed` is `func Completed(body map[string]any) Result`
— the body is encoded with sonic on send. Pass `nil` to record success
without a payload.

## 7. Concurrency, batching, lease tuning

The three knobs that matter on the worker side:

- **`Concurrency`** — number of in-flight tasks per worker process.
  Each slot is a goroutine running the handler. With Go's runtime
  scheduler a slot is cheap to keep idle, so over-provisioning is
  fine; under-provisioning leaves throughput on the table because
  every server-side claim has to wait for a free slot.
- **`BatchSize > 1`** — slots send `Ready{count: BatchSize}` and the
  server replies with up to N tasks in one `TaskBatch`. The slot then
  invokes the handler N times sequentially and coalesces the results
  into one `ResultBatch`. This amortises gRPC framing and Pebble
  commit cost across the batch. It's a win for **high-throughput**
  workloads where slots are always busy. For low-volume workloads
  it's a loss because slots wait for the server to fill the batch
  before returning anything (the server flushes partial batches once
  it's drained the queue, so the floor is one task, but the upper
  tail latency grows). `BatchSize=0` or `1` keeps the legacy
  single-task path.
- **`LeaseSeconds`** — must be strictly greater than your p99 handler
  latency, with safety margin. Too short and a slow task gets
  re-delivered to another worker while the first is still running
  ("spurious requeue"). Too long and crash recovery is slow — the
  server can't reclaim a dead worker's tasks until the lease expires.
  A common starting point: `LeaseSeconds = 2 * p99_handler_seconds`,
  then tune from there.

On the producer side, `Session.Produce` is already concurrent-safe.
The pattern is: open one `Session` at process startup, share it across
all your HTTP/RPC handlers, and let the client's sequence-number
multiplexer do the rest.

## 8. Where the gRPC connections go

```text
┌──────────────────┐                              ┌──────────────────┐
│ Producer process │── gRPC stream :9092 ────────▶│                  │
│ (your Go binary) │◀──── CreateAck / Batch ──────│                  │
└──────────────────┘                              │   codeq server   │
                                                  │  (one process,   │
┌──────────────────┐                              │   Pebble shards) │
│ Worker process   │── gRPC stream :9091 ────────▶│                  │
│ (your Go binary) │◀──── Task / TaskBatch ───────│                  │
└──────────────────┘                              └──────────────────┘
```

The two streams are independent. Producer events never traverse the
worker port and vice versa. The server splits them at the gRPC
service boundary
(`internal/producer/producerpb`, `internal/worker/workerpb`).

## 9. Real-world patterns

### HTTP service that enqueues tasks

Open the `Session` at process startup and store it on a struct that
your HTTP handlers can reach:

```go
type App struct {
    Tasks *producerclient.Session
}

func (a *App) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
    payload, _ := json.Marshal(parsedOrder)
    id, err := a.Tasks.Produce(r.Context(), producerclient.CreateRequest{
        Command:        "PROCESS_ORDER",
        Payload:        payload,
        IdempotencyKey: parsedOrder.RequestID, // dedup retries
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }
    fmt.Fprintln(w, id)
}
```

The producer harness sustains 136,392 creates/s under this exact
pattern with `ProduceBatch`
(`internal/bench/producer_stream_vs_rest_test.go::TestProducerThroughput_StreamBatchPath`).

### Background worker process

Make `worker.Client.Run` the body of your `cmd/worker/main.go`. It
blocks until the context is done, so wire it to signal handling and
let your supervisor (systemd, Kubernetes) restart the process on
exit:

```go
ctx, stop := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer stop()

if err := w.Run(ctx, handler); err != nil {
    log.Fatalf("worker: %v", err)
}
```

### Graceful shutdown semantics

When the parent context is cancelled, in-flight handler calls receive
a cancelled `ctx`. Honor it: return `workerclient.Abandon()` to
release the lease without incrementing attempts. The next worker
picks the task up with no penalty. Without that, the lease lingers
until `LeaseSeconds` expires and a slow shutdown looks like a small
outage to the producer.

## 10. Next steps

- **Multi-tenant setup** — one queue server, many tenants:
  [Multi-tenancy](./39-multi-tenancy.md).
- **HTTP API** for non-Go callers or one-off probes:
  [HTTP API](./04-http-api.md).
- **Streaming SDK reference** for the wire-level protocol:
  [Producer streaming SDK](./35-producer-streaming-sdk.md),
  [Worker streaming SDK](./36-worker-streaming-sdk.md).
- **Performance tuning** — shard count, batch sizes, lease policy:
  [Performance tuning](./17-performance-tuning.md).
- **Observability** — tracing, metrics, structured logs:
  [Observability](./37-observability.md).

## See also

- [Usage examples](./13-examples.md) — short recipes by transport.
- [Getting started](./00-getting-started.md) — single-node Pebble
  bring-up.
- [Domain model](./02-domain-model.md) — what a Task, lease, and DLQ
  actually mean inside the server.
- [`_STYLE.md` § Value proposition](./_STYLE.md#1-value-proposition).
