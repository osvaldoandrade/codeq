# Go Integration Guide

Complete guide for integrating CodeQ with Go microservices using standard library, Gin, Echo, and Fiber.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Standard Library Integration](#standard-library-integration)
- [Gin Integration](#gin-integration)
- [Worker Service](#worker-service)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Go SDK provides a context-aware, strongly-typed API with zero external dependencies for:
- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations

### Architecture

```
┌─────────────────┐         ┌─────────────┐         ┌──────────────┐
│  Go Service     │────────▶│   CodeQ     │◀────────│   Worker     │
│  (Producer)     │  HTTP   │   Server    │  HTTP   │  (Consumer)  │
└─────────────────┘         └─────────────┘         └──────────────┘
        │                           │                        │
        │                           ▼                        │
        │                    ┌─────────────┐                │
        └───────────────────▶│  KVRocks    │◀───────────────┘
                             │  (Redis)    │
                             └─────────────┘
```

## SDK Installation

```bash
go get github.com/osvaldoandrade/codeq/sdks/go
```

## Standard Library Integration

### Producer — HTTP handler that creates tasks

```go
package main

import (
    "encoding/json"
    "log"
    "net/http"
    "os"

    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

var client *codeq.Client

func main() {
    client = codeq.NewClient(
        os.Getenv("CODEQ_BASE_URL"),
        codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
    )

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

    task, err := client.CreateTask(r.Context(), codeq.CreateTaskOptions{
        Command: "PROCESS_IMAGE",
        Payload: map[string]string{"url": req.ImageURL},
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(task)
}
```

### Worker — long-running goroutine

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
    client := codeq.NewClient(
        os.Getenv("CODEQ_BASE_URL"),
        codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
    )

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    waitSec := 30
    for {
        task, err := client.ClaimTask(ctx, codeq.ClaimTaskOptions{
            Commands:    []string{"PROCESS_IMAGE"},
            WaitSeconds: &waitSec,
        })
        if err != nil {
            if ctx.Err() != nil {
                log.Println("shutting down gracefully")
                return
            }
            log.Printf("claim error: %v", err)
            continue
        }
        if task == nil {
            continue // no tasks available, loop back to long-poll
        }

        if err := processTask(ctx, client, task); err != nil {
            log.Printf("task %s failed: %v", task.ID, err)
        }
    }
}

func processTask(ctx context.Context, client *codeq.Client, task *codeq.Task) error {
    // Process the task payload…
    log.Printf("processing task %s", task.ID)

    _, err := client.SubmitResult(ctx, task.ID, codeq.SubmitResultOptions{
        Status: codeq.StatusCompleted,
        Result: map[string]string{"message": "processed successfully"},
    })
    return err
}
```

## Gin Integration

### Producer — Gin REST API

```go
package main

import (
    "net/http"
    "os"

    "github.com/gin-gonic/gin"
    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
    client := codeq.NewClient(
        os.Getenv("CODEQ_BASE_URL"),
        codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
    )

    r := gin.Default()

    r.POST("/tasks", func(c *gin.Context) {
        var req struct {
            Command string `json:"command" binding:"required"`
            Payload any    `json:"payload"`
        }
        if err := c.ShouldBindJSON(&req); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
            return
        }

        task, err := client.CreateTask(c.Request.Context(), codeq.CreateTaskOptions{
            Command: req.Command,
            Payload: req.Payload,
        })
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusCreated, task)
    })

    r.GET("/tasks/:id", func(c *gin.Context) {
        task, err := client.GetTask(c.Request.Context(), c.Param("id"))
        if err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusOK, task)
    })

    r.Run(":3000")
}
```

## Worker Service

### Worker with heartbeat and graceful shutdown

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "sync"
    "syscall"
    "time"

    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

const (
    concurrency   = 4
    heartbeatSec  = 120
    leaseSec      = 300
)

func main() {
    client := codeq.NewClient(
        os.Getenv("CODEQ_BASE_URL"),
        codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
    )

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    var wg sync.WaitGroup
    for i := 0; i < concurrency; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            workerLoop(ctx, client)
        }()
    }
    wg.Wait()
    log.Println("all workers stopped")
}

func workerLoop(ctx context.Context, client *codeq.Client) {
    lease := leaseSec
    waitSec := 30
    for {
        task, err := client.ClaimTask(ctx, codeq.ClaimTaskOptions{
            Commands:     []string{"GENERATE_REPORT"},
            LeaseSeconds: &lease,
            WaitSeconds:  &waitSec,
        })
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Printf("claim error: %v", err)
            time.Sleep(2 * time.Second)
            continue
        }
        if task == nil {
            continue
        }

        processWithHeartbeat(ctx, client, task)
    }
}

func processWithHeartbeat(ctx context.Context, client *codeq.Client, task *codeq.Task) {
    hbCtx, hbCancel := context.WithCancel(ctx)
    defer hbCancel()

    // Background heartbeat
    go func() {
        ticker := time.NewTicker(time.Duration(heartbeatSec) * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-hbCtx.Done():
                return
            case <-ticker.C:
                if err := client.Heartbeat(hbCtx, task.ID, leaseSec); err != nil {
                    log.Printf("heartbeat failed for %s: %v", task.ID, err)
                }
            }
        }
    }()

    // Simulate long-running work
    log.Printf("processing task %s", task.ID)
    time.Sleep(10 * time.Second)

    _, err := client.SubmitResult(ctx, task.ID, codeq.SubmitResultOptions{
        Status: codeq.StatusCompleted,
        Result: map[string]string{"report": "generated"},
    })
    if err != nil {
        log.Printf("submit result error for %s: %v", task.ID, err)
    }
}
```

## Best Practices

### 1. Share one Client per process

The `codeq.Client` uses Go's `http.Client` internally, which maintains its own
connection pool. Create one `Client` at startup and pass it to handlers and
workers.

```go
var client *codeq.Client

func init() {
    client = codeq.NewClient(os.Getenv("CODEQ_BASE_URL"),
        codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
        codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
    )
}
```

### 2. Use context for cancellation

Always pass the request `context.Context` so cancellations and timeouts
propagate correctly.

```go
task, err := client.CreateTask(r.Context(), opts)
```

### 3. Handle `nil` from ClaimTask

When no tasks are available (HTTP 204), `ClaimTask` returns `nil, nil`.
Always check for a nil task before processing.

```go
task, err := client.ClaimTask(ctx, opts)
if err != nil { /* handle */ }
if task == nil { continue } // nothing available
```

### 4. Use typed error checks

```go
import "errors"

var apiErr *codeq.APIError
if errors.As(err, &apiErr) && apiErr.StatusCode == 429 {
    log.Println("rate limited, backing off")
}
```

### 5. Configure retry for your environment

```go
client := codeq.NewClient(url,
    codeq.WithMaxRetries(5),
    codeq.WithRetryBaseDelay(1 * time.Second),
)
```

## Troubleshooting

### Connection refused

```
codeq: request failed: dial tcp 127.0.0.1:8080: connect: connection refused
```

**Solution**: Ensure the codeQ server is running and the `CODEQ_BASE_URL`
environment variable points to the correct address.

### Authentication errors

```
codeq: auth error: Unauthorized
```

**Solution**: Verify your JWT tokens are valid and not expired. Use different
tokens for producer, worker, and admin operations as each has a distinct scope.

### Context deadline exceeded

```
codeq: request failed: context deadline exceeded
```

**Solution**: Increase the HTTP client timeout or add a longer deadline to your
context:

```go
client := codeq.NewClient(url,
    codeq.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
)
```

### Rate limiting (429)

**Solution**: The SDK automatically retries 5xx errors but does not retry 429
responses. Add backoff logic in your application or reduce request frequency.
