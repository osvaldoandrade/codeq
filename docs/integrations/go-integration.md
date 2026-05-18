# Go Integration Guide

Complete guide for integrating CodeQ with Go services using the official
gRPC streaming clients — `pkg/producerclient` (task creation) and
`pkg/workerclient` (claim + result). Covers standalone services, Gin,
and long-running worker processes.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Standard Library Integration](#standard-library-integration)
- [Gin Integration](#gin-integration)
- [Worker Service](#worker-service)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Go clients give a context-aware, strongly-typed gRPC streaming
API for:

- **Producing tasks**: open one persistent stream, then call `Produce`
  concurrently from any goroutine. Acks return per-call.
- **Consuming tasks**: register a handler with `Run`; the client opens
  a bidirectional stream, fans out across `Concurrency` slots, and
  dispatches claimed tasks to your handler.
- **Task lifecycle**: handlers return `Completed`, `Failed`, `Nack`,
  or `Abandon` — the client coalesces results back over the stream.

### Architecture

```
┌─────────────────┐   gRPC stream    ┌─────────────┐   gRPC stream   ┌──────────────┐
│  Go Service     │ ───────────────▶ │   CodeQ     │ ◀────────────── │   Worker     │
│  (Producer)     │   :9092          │   Server    │   :9091         │  (Consumer)  │
└─────────────────┘                  └─────────────┘                 └──────────────┘
        │                                   │                                │
        │                                   ▼                                │
        │                            ┌─────────────┐                         │
        └──────────────────────────▶ │   Pebble    │ ◀───────────────────────┘
                                     │  (embedded) │
                                     └─────────────┘
```

Both clients share one HTTP/2 connection per `Client` instance, so
multiplexed Produce or Result calls from many goroutines do not open
new sockets.

## SDK Installation

```bash
go get github.com/osvaldoandrade/codeq
```

```go
import (
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)
```

The two clients live inside the main module — no separate dependency.

## Standard Library Integration

### Producer — HTTP handler that creates tasks

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "os"

    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

var prodSess *producerclient.Session

func main() {
    cli, err := producerclient.New(producerclient.Config{
        Addr:  os.Getenv("CODEQ_PRODUCER_ADDR"), // e.g. "codeq.example.com:9092"
        Token: os.Getenv("CODEQ_PRODUCER_TOKEN"),
    })
    if err != nil {
        log.Fatalf("producerclient.New: %v", err)
    }
    defer cli.Close()

    prodSess, err = cli.Connect(context.Background())
    if err != nil {
        log.Fatalf("Connect: %v", err)
    }
    defer prodSess.Close()

    http.HandleFunc("POST /tasks", createTaskHandler)
    log.Fatal(http.ListenAndServe(":3000", nil))
}

func createTaskHandler(w http.ResponseWriter, r *http.Request) {
    var req struct {
        ImageURL string `json:"imageUrl"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    payload, _ := json.Marshal(map[string]string{"url": req.ImageURL})

    taskID, err := prodSess.Produce(r.Context(), producerclient.CreateRequest{
        Command: "PROCESS_IMAGE",
        Payload: payload,
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]string{"taskId": taskID})
}
```

### Worker — long-running process

The worker client owns its own loop. You give it a `Handler`; it opens
the stream, claims tasks in batches, dispatches them across
`Concurrency` slots, and ships your results back. Returns when the
context is cancelled.

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
    w, err := workerclient.New(workerclient.Config{
        Addr:         os.Getenv("CODEQ_WORKER_ADDR"), // e.g. "codeq.example.com:9091"
        Token:        os.Getenv("CODEQ_WORKER_TOKEN"),
        Commands:     []string{"PROCESS_IMAGE"},
        Concurrency:  4,
        LeaseSeconds: 120,
    })
    if err != nil {
        log.Fatalf("workerclient.New: %v", err)
    }
    defer w.Close()

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    handler := func(ctx context.Context, t workerclient.Task) workerclient.Result {
        var p map[string]string
        if err := json.Unmarshal(t.Payload, &p); err != nil {
            return workerclient.Failed("invalid payload: " + err.Error())
        }
        log.Printf("processing task %s url=%s", t.ID, p["url"])

        // do the actual work...

        return workerclient.Completed(map[string]string{"message": "processed"})
    }

    if err := w.Run(ctx, handler); err != nil {
        log.Printf("worker exited: %v", err)
    }
}
```

## Gin Integration

### Producer — Gin REST API

```go
package main

import (
    "context"
    "encoding/json"
    "net/http"
    "os"

    "github.com/gin-gonic/gin"
    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
    cli, err := producerclient.New(producerclient.Config{
        Addr:  os.Getenv("CODEQ_PRODUCER_ADDR"),
        Token: os.Getenv("CODEQ_PRODUCER_TOKEN"),
    })
    if err != nil {
        panic(err)
    }
    defer cli.Close()

    sess, err := cli.Connect(context.Background())
    if err != nil {
        panic(err)
    }
    defer sess.Close()

    r := gin.Default()

    r.POST("/tasks", func(c *gin.Context) {
        var req struct {
            Command  string `json:"command" binding:"required"`
            Payload  any    `json:"payload"`
            Priority int    `json:"priority"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
            return
        }

        payload, _ := json.Marshal(req.Payload)
        taskID, err := sess.Produce(c.Request.Context(), producerclient.CreateRequest{
            Command:  req.Command,
            Payload:  payload,
            Priority: req.Priority,
        })
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusCreated, gin.H{"taskId": taskID})
    })

    _ = r.Run(":3000")
}
```

Reads (`GetTask`, queue stats, admin) still go through the REST API on
port 8080 — the gRPC streams are write-path only. Use a normal
`http.Client` for those.

## Worker Service

### Worker with batching and graceful shutdown

Setting `BatchSize > 1` makes each slot pull up to N tasks per Ready
and coalesce their results back as one ResultBatch — amortises gRPC
framing and Pebble commit cost across the batch.

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
    w, err := workerclient.New(workerclient.Config{
        Addr:         os.Getenv("CODEQ_WORKER_ADDR"),
        Token:        os.Getenv("CODEQ_WORKER_TOKEN"),
        Commands:     []string{"GENERATE_REPORT"},
        Concurrency:  8,
        LeaseSeconds: 300,
        BatchSize:    16, // pull up to 16 tasks per slot per cycle
    })
    if err != nil {
        log.Fatalf("workerclient.New: %v", err)
    }
    defer w.Close()

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    handler := func(ctx context.Context, t workerclient.Task) workerclient.Result {
        result, err := generateReport(ctx, t.Payload)
        if err != nil {
            // Retry-eligible error → NACK with a backoff hint (seconds).
            return workerclient.Nack(5, err.Error())
        }
        return workerclient.Completed(result)
    }

    if err := w.Run(ctx, handler); err != nil {
        log.Printf("worker exited: %v", err)
    }
    log.Println("graceful shutdown complete")
}

func generateReport(ctx context.Context, payload []byte) (any, error) {
    // ... heavy lifting ...
    return map[string]string{"report": "generated"}, nil
}
```

Lease renewal happens internally inside the worker client while your
handler is running — no need for an explicit heartbeat ticker as long
as the handler returns before `LeaseSeconds` × renewal-budget elapses.

## Best Practices

### 1. Share one Client per process

Both `producerclient.Client` and `workerclient.Client` own a single
gRPC connection. Construct once at startup, reuse across all
handlers/goroutines.

```go
var (
    prodSess *producerclient.Session
)

func init() {
    cli, _ := producerclient.New(producerclient.Config{
        Addr:  os.Getenv("CODEQ_PRODUCER_ADDR"),
        Token: os.Getenv("CODEQ_PRODUCER_TOKEN"),
    })
    prodSess, _ = cli.Connect(context.Background())
}
```

### 2. Use context for cancellation

Pass the request `context.Context` so cancellations propagate to the
gRPC call.

```go
taskID, err := prodSess.Produce(r.Context(), req)
```

### 3. Pick the right `Result` constructor

| Constructor | Semantics |
|---|---|
| `Completed(result any)` | Success. Marshal `result` as JSON. |
| `Failed(reason string)` | Terminal failure. No retry. Counts toward DLQ. |
| `Nack(backoffSec int, reason string)` | Transient. Re-queues after backoff. |
| `Abandon()` | Releases the lease without success/failure record (lets another worker pick it up immediately). |

### 4. Tune `Concurrency` + `BatchSize` together

- `Concurrency` = how many slots run in parallel inside one worker
  process.
- `BatchSize` > 1 makes each slot pull N tasks per Ready; only useful
  when the queue is consistently deep. If the queue is shallow, slots
  with batches > 1 will sit idle waiting for tasks.
- Start with `Concurrency: 8, BatchSize: 0` and bump from there.

### 5. Producer pipelining is free

A single `Session` multiplexes concurrent `Produce` calls over one
stream — goroutines can fire calls in parallel without opening new
connections. No need for client-side pooling.

## Troubleshooting

### Connection refused

```
producerclient: dial codeq.example.com:9092: connection refused
```

**Solution**: Verify the gRPC stream listener is enabled on the server
via `ProducerStreamAddr` / `WorkerStreamAddr` config (or
`PRODUCER_STREAM_ADDR` / `WORKER_STREAM_ADDR` env). These are separate
from the HTTP API port.

### Authentication errors

```
producerclient: server rejected hello: unauthenticated
```

**Solution**: Confirm the bearer token in `Config.Token` is valid for
the role. Producer and worker tokens have distinct scopes.

### Context deadline exceeded

```
producerclient: send create: context deadline exceeded
```

**Solution**: Increase the per-call context deadline or check the
server isn't backpressured. Under sustained overload `Produce` blocks
until the server acks.

### Worker idle despite tasks queued

If `Concurrency` slots are non-zero but no callbacks fire, check:

1. `Commands` matches a command actually being produced.
2. The worker JWT's `eventTypes` claim includes the command (or `*`).
3. The server's `numShards` is balanced — a worker connected to one
   node only sees tasks from shards led by that node when raft is on.

### Rate limiting

If the in-memory rate limiter is configured server-side, `Produce`
returns `rate-limited` until the next refill window. Apply local
backoff or raise the limit.
