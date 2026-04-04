# Go Integration Guide

Complete guide for integrating CodeQ with Go microservices using Gin, Echo, and stdlib frameworks.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Configuration](#configuration)
- [Gin Integration](#gin-integration)
- [Echo Integration](#echo-integration)
- [Stdlib (net/http) Integration](#stdlib-nethttp-integration)
- [Worker Implementation](#worker-implementation)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Go SDK provides an idiomatic, zero-dependency client with full support for:

- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations
- **Batch operations**: Bulk create, claim, and submit operations
- **Admin operations**: Queue statistics and cleanup

### Key Features

- **Idiomatic Go API**: Context-aware with functional options pattern
- **Zero external dependencies**: Uses only stdlib (`net/http`, `encoding/json`)
- **Full API coverage**: Producer, worker, and admin operations
- **Automatic token handling**: Built-in JWT authentication
- **Custom HTTP client**: Support for custom transports and timeouts
- **Type-safe operations**: Full type information with proper error handling

### Architecture

```
┌─────────────────┐         ┌─────────────┐         ┌──────────────┐
│  Gin/Echo/      │────────▶│   CodeQ     │◀────────│   Worker     │
│  stdlib Handler │  HTTP   │   Server    │  HTTP   │  (Consumer)  │
└─────────────────┘         └─────────────┘         └──────────────┘
        │                           │                        │
        │                           ▼                        │
        │                    ┌─────────────┐                │
        └───────────────────▶│  KVRocks    │◀───────────────┘
                             │  (Redis)    │
                             └─────────────┘
```

## SDK Installation

### Using Go Modules

Add to your `go.mod`:

```bash
go get github.com/osvaldoandrade/codeq/sdks/go
```

Or import directly in your code:

```go
import "github.com/osvaldoandrade/codeq/sdks/go"
```

### Version Requirements

- **Go 1.22+** (tested with Go 1.23+)
- No external dependencies required
- Standard library only: `net/http`, `encoding/json`, `time`, `context`

## Configuration

### Basic Client Setup

```go
package main

import (
	"context"
	"log"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
	client := codeq.NewClient(
		"http://localhost:8080",
		codeq.WithProducerToken("your-producer-token"),
		codeq.WithWorkerToken("your-worker-token"),
		codeq.WithAdminToken("your-admin-token"),
	)

	ctx := context.Background()
	
	// Client is ready to use
	_ = client
	_ = ctx
}
```

### Configuration Options

```go
// Functional options for client configuration
codeq.WithProducerToken(token string)    // JWT token for producer operations
codeq.WithWorkerToken(token string)      // JWT token for worker operations
codeq.WithAdminToken(token string)       // JWT token for admin operations
codeq.WithHTTPClient(client *http.Client) // Custom HTTP client with custom transport, timeout, etc.
```

### Custom HTTP Client

For advanced use cases (custom transport, retries, etc.):

```go
customClient := &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:       100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:    90 * time.Second,
	},
}

codeqClient := codeq.NewClient(
	"http://localhost:8080",
	codeq.WithHTTPClient(customClient),
	codeq.WithProducerToken("token"),
)
```

## Gin Integration

### 1. Setup

Create `config/codeq.go`:

```go
package config

import (
	"log"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

var CodeQClient *codeq.Client

func InitCodeQ() {
	var err error
	CodeQClient = codeq.NewClient(
		"http://localhost:8080",
		codeq.WithProducerToken("your-producer-token"),
		codeq.WithWorkerToken("your-worker-token"),
	)
	if err != nil {
		log.Fatalf("Failed to initialize CodeQ client: %v", err)
	}
}
```

### 2. Producer Routes

Create `routes/tasks.go`:

```go
package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"your-app/config"
	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

type CreateTaskRequest struct {
	Command     string         `json:"command" binding:"required"`
	Payload     map[string]any `json:"payload" binding:"required"`
	Priority    *int           `json:"priority"`
	MaxAttempts *int           `json:"maxAttempts"`
	DelaySeconds *int          `json:"delaySeconds"`
	Webhook     string         `json:"webhook"`
}

// POST /api/tasks
func CreateTask(c *gin.Context) {
	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	task, err := config.CodeQClient.CreateTask(c.Request.Context(), &codeq.CreateTaskOptions{
		Command:      req.Command,
		Payload:      req.Payload,
		Priority:     req.Priority,
		MaxAttempts:  req.MaxAttempts,
		DelaySeconds: req.DelaySeconds,
		Webhook:      req.Webhook,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, task)
}

// GET /api/tasks/:id
func GetTask(c *gin.Context) {
	taskID := c.Param("id")

	task, err := config.CodeQClient.GetTask(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Task not found"})
		return
	}

	c.JSON(http.StatusOK, task)
}

// GET /api/tasks/:id/result
func GetResult(c *gin.Context) {
	taskID := c.Param("id")

	result, err := config.CodeQClient.GetResult(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Result not found"})
		return
	}

	c.JSON(http.StatusOK, result)
}
```

Register routes in `main.go`:

```go
package main

import (
	"github.com/gin-gonic/gin"
	"your-app/config"
	"your-app/routes"
)

func main() {
	config.InitCodeQ()

	engine := gin.Default()

	// Task producer routes
	api := engine.Group("/api")
	{
		api.POST("/tasks", routes.CreateTask)
		api.GET("/tasks/:id", routes.GetTask)
		api.GET("/tasks/:id/result", routes.GetResult)
	}

	engine.Run(":8888")
}
```

### 3. Worker Implementation (Gin Background Service)

Create `workers/task_worker.go`:

```go
package workers

import (
	"context"
	"log"
	"time"

	"github.com/gin-gonic/gin"
	codeq "github.com/osvaldoandrade/codeq/sdks/go"
	"your-app/config"
)

const (
	workerID       = "gin-worker-1"
	pollInterval   = 5 * time.Second
	leaseDuration  = 300 // 5 minutes
	pollTimeout    = 10  // long-poll timeout in seconds
)

type WorkerPool struct {
	client   *codeq.Client
	commands []string
	stopCh   chan struct{}
	done     chan struct{}
}

func NewWorkerPool(commands []string) *WorkerPool {
	return &WorkerPool{
		client:   config.CodeQClient,
		commands: commands,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins polling for tasks
func (wp *WorkerPool) Start(ctx context.Context) {
	go wp.poll(ctx)
}

// Stop gracefully shuts down the worker pool
func (wp *WorkerPool) Stop() {
	close(wp.stopCh)
	<-wp.done
}

func (wp *WorkerPool) poll(ctx context.Context) {
	defer close(wp.done)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-wp.stopCh:
			return
		case <-ticker.C:
			wp.claimAndProcess(ctx)
		}
	}
}

func (wp *WorkerPool) claimAndProcess(ctx context.Context) {
	claimed, err := wp.client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
		Commands:     wp.commands,
		LeaseSeconds: codeq.Int(leaseDuration),
		WaitSeconds:  codeq.Int(pollTimeout),
	})

	if err != nil {
		log.Printf("Failed to claim task: %v", err)
		return
	}

	if claimed == nil {
		return // No tasks available
	}

	// Process the task
	if err := wp.processTask(ctx, claimed); err != nil {
		log.Printf("Failed to process task %s: %v", claimed.ID, err)

		// NACK the task to retry
		if err := wp.nackTask(ctx, claimed.ID); err != nil {
			log.Printf("Failed to NACK task %s: %v", claimed.ID, err)
		}
		return
	}

	// Task completed successfully
	if _, err := wp.client.SubmitResult(ctx, claimed.ID, &codeq.SubmitResultOptions{
		Status: "COMPLETED",
		Result: map[string]any{"processed": true},
	}); err != nil {
		log.Printf("Failed to submit result for task %s: %v", claimed.ID, err)
	}
}

func (wp *WorkerPool) processTask(ctx context.Context, task *codeq.Task) error {
	// Simulate task processing
	log.Printf("Processing task %s (command: %s)", task.ID, task.Command)

	// Start heartbeat ticker to refresh lease
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer heartbeatTicker.Stop()

	processingDone := make(chan error, 1)

	go func() {
		// Simulate processing work
		time.Sleep(2 * time.Second)
		processingDone <- nil
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-processingDone:
			return err
		case <-heartbeatTicker.C:
			if err := wp.heartbeat(ctx, task.ID); err != nil {
				log.Printf("Failed to send heartbeat for task %s: %v", task.ID, err)
			}
		}
	}
}

func (wp *WorkerPool) heartbeat(ctx context.Context, taskID string) error {
	return wp.client.Heartbeat(ctx, taskID)
}

func (wp *WorkerPool) nackTask(ctx context.Context, taskID string) error {
	return wp.client.NackTask(ctx, taskID)
}
```

### 4. Main Application

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"your-app/config"
	"your-app/routes"
	"your-app/workers"
)

func main() {
	config.InitCodeQ()

	// Initialize worker pool
	workerPool := workers.NewWorkerPool([]string{"PROCESS_TASK", "GENERATE_REPORT"})
	workerPool.Start(context.Background())

	// Setup HTTP server
	engine := gin.Default()

	// Register routes
	api := engine.Group("/api")
	{
		api.POST("/tasks", routes.CreateTask)
		api.GET("/tasks/:id", routes.GetTask)
		api.GET("/tasks/:id/result", routes.GetResult)
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		workerPool.Stop()
		os.Exit(0)
	}()

	if err := engine.Run(":8888"); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
```

## Echo Integration

### 1. Setup

Create `config/codeq.go` (same as Gin):

```go
package config

import (
	"log"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

var CodeQClient *codeq.Client

func InitCodeQ() {
	CodeQClient = codeq.NewClient(
		"http://localhost:8080",
		codeq.WithProducerToken("your-producer-token"),
		codeq.WithWorkerToken("your-worker-token"),
	)
}
```

### 2. Producer Routes

Create `routes/tasks.go`:

```go
package routes

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"your-app/config"
	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

type CreateTaskRequest struct {
	Command      string         `json:"command" validate:"required"`
	Payload      map[string]any `json:"payload" validate:"required"`
	Priority     *int           `json:"priority"`
	MaxAttempts  *int           `json:"maxAttempts"`
	DelaySeconds *int           `json:"delaySeconds"`
	Webhook      string         `json:"webhook"`
}

// POST /api/tasks
func CreateTask(c echo.Context) error {
	var req CreateTaskRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	task, err := config.CodeQClient.CreateTask(c.Request().Context(), &codeq.CreateTaskOptions{
		Command:      req.Command,
		Payload:      req.Payload,
		Priority:     req.Priority,
		MaxAttempts:  req.MaxAttempts,
		DelaySeconds: req.DelaySeconds,
		Webhook:      req.Webhook,
	})

	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, task)
}

// GET /api/tasks/:id
func GetTask(c echo.Context) error {
	taskID := c.Param("id")

	task, err := config.CodeQClient.GetTask(c.Request().Context(), taskID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Task not found"})
	}

	return c.JSON(http.StatusOK, task)
}

// GET /api/tasks/:id/result
func GetResult(c echo.Context) error {
	taskID := c.Param("id")

	result, err := config.CodeQClient.GetResult(c.Request().Context(), taskID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Result not found"})
	}

	return c.JSON(http.StatusOK, result)
}
```

### 3. Main Application

```go
package main

import (
	"log"

	"github.com/labstack/echo/v4"
	"your-app/config"
	"your-app/routes"
	"your-app/workers"
	"context"
)

func main() {
	config.InitCodeQ()

	// Initialize worker pool
	workerPool := workers.NewWorkerPool([]string{"PROCESS_TASK", "GENERATE_REPORT"})
	workerPool.Start(context.Background())

	// Setup Echo server
	e := echo.New()

	// Register routes
	api := e.Group("/api")
	{
		api.POST("/tasks", routes.CreateTask)
		api.GET("/tasks/:id", routes.GetTask)
		api.GET("/tasks/:id/result", routes.GetResult)
	}

	if err := e.Start(":8888"); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
```

## Stdlib (net/http) Integration

For applications using only stdlib `net/http` without external frameworks:

### 1. Setup

```go
package main

import (
	"context"
	"log"
	"net/http"
	"encoding/json"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

var client *codeq.Client

func init() {
	client = codeq.NewClient(
		"http://localhost:8080",
		codeq.WithProducerToken("your-producer-token"),
		codeq.WithWorkerToken("your-worker-token"),
	)
}
```

### 2. HTTP Handlers

```go
func createTaskHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Command  string         `json:"command"`
		Payload  map[string]any `json:"payload"`
		Priority *int           `json:"priority"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task, err := client.CreateTask(r.Context(), &codeq.CreateTaskOptions{
		Command:  req.Command,
		Payload:  req.Payload,
		Priority: req.Priority,
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

func main() {
	http.HandleFunc("/api/tasks", createTaskHandler)
	log.Fatal(http.ListenAndServe(":8888", nil))
}
```

## Worker Implementation

### Standalone Worker Process

For a dedicated worker service:

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
	client := codeq.NewClient(
		"http://localhost:8080",
		codeq.WithWorkerToken("your-worker-token"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	// Start polling
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processTasks(ctx, client)
		}
	}
}

func processTasks(ctx context.Context, client *codeq.Client) {
	// Claim a task
	task, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
		Commands:     []string{"PROCESS_TASK", "GENERATE_REPORT"},
		LeaseSeconds: codeq.Int(300),
		WaitSeconds:  codeq.Int(10),
	})

	if err != nil {
		log.Printf("Failed to claim task: %v", err)
		return
	}

	if task == nil {
		return // No tasks available
	}

	log.Printf("Processing task %s (command: %s)", task.ID, task.Command)

	// Simulate work
	time.Sleep(2 * time.Second)

	// Submit result
	_, err = client.SubmitResult(ctx, task.ID, &codeq.SubmitResultOptions{
		Status: "COMPLETED",
		Result: map[string]any{"output": "processed"},
	})

	if err != nil {
		log.Printf("Failed to submit result: %v", err)
		// NACK on failure to retry
		client.NackTask(ctx, task.ID)
	}
}
```

### Batch Operations

```go
// Create multiple tasks in a single request
tasks, err := client.CreateTasksBatch(ctx, []codeq.CreateTaskOptions{
	{
		Command: "PROCESS_TASK",
		Payload: map[string]any{"id": "1"},
	},
	{
		Command: "PROCESS_TASK",
		Payload: map[string]any{"id": "2"},
	},
})

if err != nil {
	log.Fatalf("Failed to create batch: %v", err)
}

// Batch claim tasks
claimed, err := client.BatchClaimTasks(ctx, &codeq.BatchClaimOptions{
	Commands:     []string{"PROCESS_TASK"},
	LeaseSeconds: codeq.Int(300),
	Count:        codeq.Int(10),
})

if err != nil {
	log.Fatalf("Failed to claim batch: %v", err)
}

// Batch submit results
_, err = client.BatchSubmitResults(ctx, []codeq.BatchSubmitResultsEntry{
	{
		TaskID: "task-1",
		Result: &codeq.SubmitResultOptions{
			Status: "COMPLETED",
			Result: map[string]any{"output": "done"},
		},
	},
})

if err != nil {
	log.Fatalf("Failed to submit results: %v", err)
}
```

## Best Practices

### 1. Error Handling

```go
task, err := client.CreateTask(ctx, opts)
if err != nil {
	// Handle different error types
	var codErr *codeq.CodeQError
	if errors.As(err, &codErr) {
		if codErr.IsRetryable() {
			// Implement exponential backoff
			time.Sleep(time.Duration(math.Pow(2, float64(attempt))) * time.Second)
			// Retry...
		}
	}
	return err
}
```

### 2. Context Management

Always use context with timeouts:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

task, err := client.CreateTask(ctx, opts)
```

### 3. Worker Pool Size

Tune pool size based on throughput:

```go
// Concurrent workers = (LeaseSeconds / ProcessTime) + buffer
// Example: 5-minute lease, 1s processing = 300 concurrent max
// Typical pool: 50-100 workers for balanced throughput
```

### 4. Heartbeat Strategy

Send heartbeats at ~50% of lease duration:

```go
heartbeatInterval := time.Duration(leaseSeconds/2) * time.Second
heartbeatTicker := time.NewTicker(heartbeatInterval)
```

### 5. Idempotency

Use idempotency keys for deduplication:

```go
task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
	Command:        "PROCESS_INVOICE",
	Payload:        invoice,
	IdempotencyKey: fmt.Sprintf("invoice-%s", invoice.ID),
})
```

### 6. Resource Limits

Set appropriate timeouts and connection limits:

```go
customClient := &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost:  10,
		MaxConnsPerHost:      100,
		IdleConnTimeout:      90 * time.Second,
		DisableCompression:   false,
		DisableKeepAlives:    false,
	},
}

codeqClient := codeq.NewClient(baseURL, codeq.WithHTTPClient(customClient))
```

## Troubleshooting

### Connection Issues

**Problem**: `connection refused` errors

**Solution**: Verify CodeQ server is running:

```bash
curl -v http://localhost:8080/health
```

Check network connectivity:

```go
// Add request logging
transport := &http.Transport{/* config */}
transport.Proxy = http.ProxyFromEnvironment

// Test with verbose client
client := codeq.NewClient(baseURL, codeq.WithHTTPClient(&http.Client{
	Transport: transport,
	Timeout:   10 * time.Second,
}))
```

### Authentication Failures

**Problem**: `401 Unauthorized` errors

**Solution**: Verify tokens are set:

```go
// Check token format (JWT)
parts := strings.Split(token, ".")
if len(parts) != 3 {
	log.Fatal("Invalid JWT format")
}

// Use correct token for operation:
// - Producer operations: WithProducerToken
// - Worker operations: WithWorkerToken
// - Admin operations: WithAdminToken
```

### High Latency

**Problem**: Slow task creation/claiming

**Solution**: Use batch operations:

```go
// Instead of:
for _, item := range items {
	client.CreateTask(ctx, opts)
}

// Use:
client.CreateTasksBatch(ctx, optsSlice)
```

### Worker Not Claiming Tasks

**Problem**: No tasks are being processed

**Solution**: Check task command filters:

```go
// Ensure worker commands match task commands
claimedTask, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
	Commands: []string{"EXACT_COMMAND_NAME"}, // Must match exactly
})

// Verify task status:
task, _ := client.GetTask(ctx, taskID)
log.Printf("Task status: %v", task.Status)
```

### Memory Leaks in Worker Loop

**Problem**: Growing memory usage over time

**Solution**: Ensure proper context cleanup:

```go
for {
	select {
	case <-ctx.Done():
		return // Clean shutdown
	case <-ticker.C:
		// Create new context for each operation
		opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		processTasks(opCtx, client)
		cancel() // Important: cancel context
	}
}
```

## Additional Resources

- [Go SDK API Reference](https://pkg.go.dev/github.com/osvaldoandrade/codeq/sdks/go)
- [CodeQ HTTP API Documentation](../04-http-api.md)
- [Performance Tuning Guide](../17-performance-tuning.md)
- [Testing Guide](../19-testing.md)
- [Examples](../../examples/)
