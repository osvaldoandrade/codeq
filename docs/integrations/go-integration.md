# Go Integration Guide

Complete guide for integrating CodeQ with Go microservices using Gin, Echo, and standard library.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Gin Integration](#gin-integration)
- [Echo Integration](#echo-integration)
- [Standard Library Integration](#standard-library-integration)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Go SDK provides a simple, idiomatic API with zero external dependencies for:
- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations
- **Webhooks**: Subscribe to task completion events

### Architecture

````
┌─────────────────┐         ┌─────────────┐         ┌──────────────┐
│  Microservice   │────────▶│   CodeQ     │◀────────│   Worker     │
│  (Producer)     │  HTTP   │   Server    │  HTTP   │  (Consumer)  │
└─────────────────┘         └─────────────┘         └──────────────┘
        │                           │                        │
        │                           ▼                        │
        │                    ┌─────────────┐                │
        └───────────────────▶│  KVRocks    │◀───────────────┘
                             │  (Redis)    │
                             └─────────────┘
````

## SDK Installation

### Go Modules

Add to your `go.mod`:

```bash
go get github.com/osvaldoandrade/codeq-sdk-go
```

Or with a specific version:

```bash
go get github.com/osvaldoandrade/codeq-sdk-go@v1.0.0
```

### Zero Dependencies

The Go SDK has zero external dependencies and uses only the Go standard library:
- `net/http` for HTTP client
- `encoding/json` for JSON marshaling
- `context` for cancellation and timeouts

## Gin Integration

### 1. Configuration

Create `config/codeq.go`:

```go
package config

import (
	"github.com/osvaldoandrade/codeq-sdk-go"
	"os"
)

func NewCodeQClient() *codeq.Client {
	client := codeq.NewClient(
		getEnv("CODEQ_BASE_URL", "http://localhost:8080"),
		codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
		codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
	)
	return client
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
```

### 2. Producer Routes

Create `routes/tasks.go`:

```go
package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/osvaldoandrade/codeq-sdk-go"
	"myapp/models"
	"net/http"
)

func SetupTaskRoutes(r *gin.Engine, client *codeq.Client) {
	tasks := r.Group("/api/tasks")
	{
		tasks.POST("", func(c *gin.Context) {
			var req models.TaskRequest
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			// Create a task
			task, err := client.CreateTask(c.Request.Context(), &codeq.CreateTaskOptions{
				Command:  req.Command,
				Payload:  req.Payload,
				Priority: codeq.Int(5),
				Webhook:  "https://example.com/callbacks/results",
			})
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusCreated, task)
		})
	}
}
```

### 3. Worker Service

Create `services/worker.go`:

```go
package services

import (
	"context"
	"fmt"
	"github.com/osvaldoandrade/codeq-sdk-go"
	"log"
	"time"
)

type TaskWorker struct {
	client *codeq.Client
}

func NewTaskWorker(client *codeq.Client) *TaskWorker {
	return &TaskWorker{client: client}
}

// Start begins polling for tasks
func (w *TaskWorker) Start(ctx context.Context, commands []string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollTasks(ctx, commands)
		}
	}
}

func (w *TaskWorker) pollTasks(ctx context.Context, commands []string) {
	// Claim a task with long polling
	task, err := w.client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
		Commands:     commands,
		LeaseSeconds: codeq.Int(120),
		WaitSeconds:  codeq.Int(10),
	})
	if err != nil {
		log.Printf("Error claiming task: %v", err)
		return
	}

	if task == nil {
		// No tasks available
		return
	}

	// Process the task
	result, err := w.processTask(ctx, task)
	if err != nil {
		// Submit a failed result
		_, err = w.client.SubmitResult(ctx, task.ID, &codeq.SubmitResultOptions{
			Status: "FAILED",
			Error:  err.Error(),
		})
		if err != nil {
			log.Printf("Error submitting failed result: %v", err)
		}
		return
	}

	// Submit the result
	_, err = w.client.SubmitResult(ctx, task.ID, &codeq.SubmitResultOptions{
		Status: "COMPLETED",
		Result: result,
	})
	if err != nil {
		log.Printf("Error submitting result: %v", err)
	}
}

func (w *TaskWorker) processTask(ctx context.Context, task *codeq.Task) (map[string]any, error) {
	// Your business logic here
	log.Printf("Processing task: %s with command: %s", task.ID, task.Command)

	// Example: process the payload
	payload, ok := task.Payload.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid payload format")
	}

	// Do work...
	jobID := payload["jobId"].(string)
	log.Printf("Processing job: %s", jobID)

	return map[string]any{
		"jobId":   jobID,
		"status":  "processed",
		"message": "Task completed successfully",
	}, nil
}
```

### 4. Main Application

Update `main.go`:

```go
package main

import (
	"context"
	"github.com/gin-gonic/gin"
	"myapp/config"
	"myapp/routes"
	"myapp/services"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Initialize CodeQ client
	codeQClient := config.NewCodeQClient()

	// Setup Gin router
	router := gin.Default()
	routes.SetupTaskRoutes(router, codeQClient)

	// Start worker service in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := services.NewTaskWorker(codeQClient)
	go worker.Start(ctx, []string{"PROCESS_JOB", "GENERATE_REPORT"})

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		cancel()
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}()

	router.Run(":3000")
}
```

## Echo Integration

### 1. Configuration

Same as Gin - use `config/codeq.go` from above.

### 2. Producer Routes

Create `routes/tasks.go`:

```go
package routes

import (
	"github.com/labstack/echo/v4"
	"github.com/osvaldoandrade/codeq-sdk-go"
	"myapp/models"
	"net/http"
)

func SetupTaskRoutes(e *echo.Echo, client *codeq.Client) {
	e.POST("/api/tasks", func(c echo.Context) error {
		var req models.TaskRequest
		if err := c.Bind(&req); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}

		// Create a task
		task, err := client.CreateTask(c.Request().Context(), &codeq.CreateTaskOptions{
			Command:  req.Command,
			Payload:  req.Payload,
			Priority: codeq.Int(5),
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		return c.JSON(http.StatusCreated, task)
	})
}
```

### 3. Main Application

```go
package main

import (
	"context"
	"github.com/labstack/echo/v4"
	"myapp/config"
	"myapp/routes"
	"myapp/services"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	codeQClient := config.NewCodeQClient()

	e := echo.New()
	routes.SetupTaskRoutes(e, codeQClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := services.NewTaskWorker(codeQClient)
	go worker.Start(ctx, []string{"PROCESS_JOB"})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	e.Logger.Fatal(e.Start(":3000"))
}
```

## Standard Library Integration

Using only `net/http` and no framework:

### 1. Simple HTTP Server

```go
package main

import (
	"context"
	"encoding/json"
	"github.com/osvaldoandrade/codeq-sdk-go"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// Initialize CodeQ client
	client := codeq.NewClient(
		os.Getenv("CODEQ_BASE_URL"),
		codeq.WithProducerToken(os.Getenv("CODEQ_PRODUCER_TOKEN")),
		codeq.WithWorkerToken(os.Getenv("CODEQ_WORKER_TOKEN")),
	)

	// Setup routes
	mux := http.NewServeMux()

	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		task, err := client.CreateTask(r.Context(), &codeq.CreateTaskOptions{
			Command: "PROCESS_JOB",
			Payload: payload,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(task)
	})

	// Start worker goroutine
	ctx, cancel := context.WithCancel(context.Background())
	go startWorker(ctx, client)

	// Handle graceful shutdown
	server := &http.Server{
		Addr:    ":3000",
		Handler: mux,
	}

	log.Printf("Server starting on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}

	cancel()
}

func startWorker(ctx context.Context, client *codeq.Client) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			task, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
				Commands: []string{"PROCESS_JOB"},
			})
			if err != nil {
				log.Printf("Error claiming task: %v", err)
				continue
			}

			if task != nil {
				// Process task...
				client.SubmitResult(ctx, task.ID, &codeq.SubmitResultOptions{
					Status: "COMPLETED",
					Result: map[string]any{"processed": true},
				})
			}
		}
	}
}
```

## Best Practices

### 1. Context Management

Always use contexts for cancellation and timeouts:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
	Command: "SEND_EMAIL",
	Payload: payload,
})
```

### 2. Error Handling

Handle SDK errors gracefully:

```go
task, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
	Commands: []string{"PROCESS"},
})
if err != nil {
	// Check if this is a timeout or other error
	log.Printf("Error claiming task: %v", err)
	return
}

// task is nil if no tasks available (NoContent response)
if task == nil {
	log.Println("No tasks available")
	return
}
```

### 3. Batch Operations

Use batch operations for better throughput:

```go
// Batch create tasks
results, err := client.CreateTasksBatch(ctx, []codeq.CreateTaskOptions{
	{Command: "SEND_EMAIL", Payload: map[string]any{"email": "user1@example.com"}},
	{Command: "SEND_EMAIL", Payload: map[string]any{"email": "user2@example.com"}},
	{Command: "SEND_EMAIL", Payload: map[string]any{"email": "user3@example.com"}},
})
if err != nil {
	log.Printf("Error in batch create: %v", err)
	return
}

for _, result := range results {
	if result.Error != "" {
		log.Printf("Failed to create task: %s", result.Error)
		continue
	}
	log.Printf("Created task: %s", result.Task.ID)
}
```

### 4. Heartbeat for Long-Running Tasks

Keep the lease alive during processing:

```go
task, _ := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
	Commands:     []string{"LONG_PROCESS"},
	LeaseSeconds: codeq.Int(60),
})

go func() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if err := client.Heartbeat(ctx, task.ID, 60); err != nil {
			log.Printf("Heartbeat failed: %v", err)
			break
		}
	}
}()

// Do long work...
result, _ := client.SubmitResult(ctx, task.ID, &codeq.SubmitResultOptions{
	Status: "COMPLETED",
	Result: map[string]any{"processed": true},
})
```

### 5. Webhook Subscriptions

Subscribe to task completion events:

```go
sub, err := client.CreateSubscription(ctx, &codeq.CreateSubscriptionOptions{
	CallbackURL: "https://example.com/webhooks/tasks",
	EventTypes:  []string{"SEND_EMAIL", "GENERATE_REPORT"},
	TTLSeconds:  codeq.Int(3600),
	DeliveryMode: "fanout",
})
if err != nil {
	log.Printf("Error creating subscription: %v", err)
	return
}

log.Printf("Subscription created: %s (expires at %s)", sub.SubscriptionID, sub.ExpiresAt)

// Renew subscription before expiration
_, err = client.RenewSubscription(ctx, sub.SubscriptionID, &codeq.RenewSubscriptionOptions{
	TTLSeconds: codeq.Int(3600),
})
```

### 6. Idempotent Task Creation

Prevent duplicate task creation:

```go
task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
	Command:        "SEND_EMAIL",
	Payload:        map[string]any{"email": "user@example.com"},
	IdempotencyKey: "email_user_20240101",
})
```

### 7. Scheduled Tasks

Schedule tasks for future execution:

```go
runAt := time.Now().Add(1 * time.Hour).Format(time.RFC3339)

task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
	Command: "GENERATE_REPORT",
	Payload: map[string]any{"report_id": "123"},
	RunAt:   runAt,
})
```

## Troubleshooting

### Connection Errors

**Problem**: `connection refused` or `no such host`

**Solution**: Verify CodeQ server is running and accessible:

```bash
curl -i http://localhost:8080/health
```

Check environment variables:

```bash
echo $CODEQ_BASE_URL
echo $CODEQ_PRODUCER_TOKEN
```

### Authentication Errors

**Problem**: `401 Unauthorized`

**Solution**: Verify tokens are set and correct:

```go
if producerToken == "" {
	return fmt.Errorf("CODEQ_PRODUCER_TOKEN not set")
}

client := codeq.NewClient(
	os.Getenv("CODEQ_BASE_URL"),
	codeq.WithProducerToken(producerToken),
)
```

### Claiming No Tasks

**Problem**: `ClaimTask` always returns nil

**Solution**: Verify tasks exist in the queue:

```go
// Use admin token to get queue stats
stats, err := client.GetQueueStats(ctx, "PROCESS_JOB")
if err != nil {
	log.Printf("Error: %v", err)
	return
}
log.Printf("Queue stats: %+v", stats)
```

### Context Timeouts

**Problem**: Requests hanging or timing out

**Solution**: Use appropriate context timeouts:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

task, err := client.CreateTask(ctx, opts)
if err != nil {
	log.Printf("Timeout or error: %v", err)
}
```

### Lease Expiration

**Problem**: Task leases expiring during processing

**Solution**: Increase lease duration or send heartbeats:

```go
task, _ := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
	Commands:     []string{"LONG_PROCESS"},
	LeaseSeconds: codeq.Int(300), // 5 minutes
})

// Send heartbeat every 2 minutes
go func() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		client.Heartbeat(ctx, task.ID, 300)
	}
}()
```
