# codeQ Go SDK

Official Go client SDK for [codeQ](https://github.com/osvaldoandrade/codeq) — a distributed task queue.

## Features

- **Zero dependencies** — uses only the Go standard library
- **Full API coverage** — producer, worker, admin, and subscription operations
- **Context support** — all methods accept `context.Context` for cancellation and timeouts
- **Automatic retry** — configurable exponential back-off for transient failures (5xx)
- **Strong typing** — Go structs for all request/response models
- **Thread-safe** — safe for concurrent use from multiple goroutines

## Requirements

- Go 1.22 or later
- A running codeQ server

## Installation

```bash
go get github.com/osvaldoandrade/codeq/sdks/go
```

## Quick Start

### Producer — create tasks

```go
package main

import (
    "context"
    "fmt"
    "log"

    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
    client := codeq.NewClient("http://localhost:8080",
        codeq.WithProducerToken("your-producer-jwt"),
    )

    task, err := client.CreateTask(context.Background(), codeq.CreateTaskOptions{
        Command: "PROCESS_IMAGE",
        Payload: map[string]any{"url": "https://example.com/image.png"},
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Created task %s (status: %s)\n", task.ID, task.Status)
}
```

### Worker — claim and complete tasks

```go
package main

import (
    "context"
    "fmt"
    "log"

    codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
    client := codeq.NewClient("http://localhost:8080",
        codeq.WithWorkerToken("your-worker-jwt"),
    )

    waitSec := 30
    task, err := client.ClaimTask(context.Background(), codeq.ClaimTaskOptions{
        Commands:    []string{"PROCESS_IMAGE"},
        WaitSeconds: &waitSec,
    })
    if err != nil {
        log.Fatal(err)
    }
    if task == nil {
        fmt.Println("No tasks available")
        return
    }

    fmt.Printf("Claimed task %s\n", task.ID)

    // Process the task …

    result, err := client.SubmitResult(context.Background(), task.ID, codeq.SubmitResultOptions{
        Status: codeq.StatusCompleted,
        Result: map[string]any{"output": "processed"},
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Result submitted for %s\n", result.TaskID)
}
```

## API Reference

### Client Configuration

```go
client := codeq.NewClient(baseURL string, opts ...codeq.Option)
```

| Option | Description |
|--------|-------------|
| `WithProducerToken(t)` | JWT for producer endpoints |
| `WithWorkerToken(t)` | JWT for worker endpoints |
| `WithAdminToken(t)` | JWT for admin endpoints |
| `WithHTTPClient(hc)` | Custom `*http.Client` |
| `WithMaxRetries(n)` | Max retry attempts (default: 3) |
| `WithRetryBaseDelay(d)` | Base delay for exponential back-off (default: 500 ms) |

### Producer Operations

| Method | Description |
|--------|-------------|
| `CreateTask(ctx, opts)` | Create a single task |
| `CreateTasksBatch(ctx, tasks)` | Create up to 100 tasks |

### Worker Operations

| Method | Description |
|--------|-------------|
| `ClaimTask(ctx, opts)` | Claim a task (returns `nil` when none available) |
| `ClaimTasksBatch(ctx, opts)` | Claim up to 10 tasks |
| `SubmitResult(ctx, taskID, opts)` | Submit task result |
| `SubmitResultsBatch(ctx, subs)` | Submit up to 100 results |
| `Heartbeat(ctx, taskID, secs)` | Extend task lease |
| `Abandon(ctx, taskID)` | Return task to queue |
| `Nack(ctx, taskID, delay, reason)` | Negative ack with retry delay |

### Subscription Operations

| Method | Description |
|--------|-------------|
| `CreateSubscription(ctx, opts)` | Register a webhook subscription |
| `RenewSubscription(ctx, id, opts)` | Renew subscription lifetime |

### Query Operations

| Method | Description |
|--------|-------------|
| `GetTask(ctx, taskID)` | Get task details |
| `GetResult(ctx, taskID)` | Get task result |
| `WaitForResult(ctx, taskID, opts)` | Poll until result is available |

### Admin Operations

| Method | Description |
|--------|-------------|
| `ListQueues(ctx)` | List all queue statistics |
| `GetQueueStats(ctx, command)` | Get stats for a single queue |
| `CleanupExpired(ctx, opts)` | Remove expired tasks |

## Error Handling

The SDK defines four error types:

```go
// Base error wrapping a root cause
var sdkErr *codeq.Error

// HTTP 4xx/5xx response from the API
var apiErr *codeq.APIError

// HTTP 401 or 403
var authErr *codeq.AuthError

// WaitForResult or context deadline exceeded
var timeoutErr *codeq.TimeoutError
```

Use `errors.As` for type checks:

```go
import "errors"

task, err := client.CreateTask(ctx, opts)
if err != nil {
    var apiErr *codeq.APIError
    if errors.As(err, &apiErr) {
        fmt.Printf("HTTP %d: %s\n", apiErr.StatusCode, apiErr.ResponseBody)
    }
}
```

## Configuration via Environment Variables

A common pattern is to configure the client from environment variables:

```go
client := codeq.NewClient(
    os.Getenv("CODEQ_BASE_URL"),
    codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
    codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
)
```

## Retry Behaviour

By default the client retries up to 3 times with exponential back-off
(500 ms, 1 s, 2 s) on 5xx server errors and network failures. Client errors
(4xx) are never retried.

```go
// Disable retries
client := codeq.NewClient(url, codeq.WithMaxRetries(0))

// Custom back-off
client := codeq.NewClient(url,
    codeq.WithMaxRetries(5),
    codeq.WithRetryBaseDelay(1 * time.Second),
)
```

## Testing

```bash
go test -v ./...
```

## License

This SDK is part of the [codeQ](https://github.com/osvaldoandrade/codeq) project and is released under the same license.
