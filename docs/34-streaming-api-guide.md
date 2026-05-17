# Streaming API Guide

This guide covers codeQ's gRPC streaming APIs for producers and workers. Streaming provides **2–3× higher throughput** than REST by enabling pipelined async operations on a single persistent connection, amortizing authentication overhead, and reducing latency variance.

## Quick Start

### Producer Streaming (Submit Tasks)

Submit tasks at ~33,000 ops/s per connection:

````go
package main

import (
	"context"
	"log"
	"github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
	client, err := producerclient.New(producerclient.Config{
		Addr:  "codeq.example.com:9092",
		Token: "your-bearer-token",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	session, err := client.Connect(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	// Send 1000 tasks concurrently
	for i := 0; i < 1000; i++ {
		go func(idx int) {
			ack, err := session.Produce(context.Background(), producerclient.CreateTaskRequest{
				Command: "my-task",
				Payload: []byte(`{"data":"value"}`),
			})
			if err != nil {
				log.Printf("produce failed: %v", err)
				return
			}
			log.Printf("Task %d created as %s", idx, ack.TaskID)
		}(i)
	}
}
````

### Worker Streaming (Claim & Process Tasks)

Process tasks at variable throughput with configurable concurrency:

````go
package main

import (
	"context"
	"log"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
	client, err := workerclient.New(workerclient.Config{
		Addr:        "codeq.example.com:9091",
		Token:       "your-bearer-token",
		Concurrency: 10, // process up to 10 tasks in parallel
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
		log.Printf("Processing task %s: %s", task.ID, task.Command)
		
		// Do work...
		
		// Return result
		return workerclient.Completed(map[string]any{
			"status": "done",
		})
	}

	if err := client.Run(context.Background(), handler); err != nil {
		log.Fatal(err)
	}
}
````

---

## Tutorials

### Tutorial 1: Producer Streaming — Your First Stream

Learn to submit tasks via streaming. You'll create 100 tasks in parallel and verify they arrive server-side.

**Goal:** Send tasks faster than REST allows, with pipelined async acknowledgments.

**Prerequisites:**
- codeQ server running on `localhost:9092`
- A valid bearer token (see authentication docs)

**Steps:**

1. **Dial the server:**
   ````go
   import "github.com/osvaldoandrade/codeq/pkg/producerclient"
   
   client, err := producerclient.New(producerclient.Config{
   	Addr:  "localhost:9092",
   	Token: os.Getenv("CODEQ_TOKEN"),
   })
   if err != nil {
       return err
   }
   defer client.Close()
   ````

2. **Open a stream session:**
   ````go
   session, err := client.Connect(context.Background())
   if err != nil {
       return err
   }
   ````

3. **Send tasks concurrently:**
   ````go
   var wg sync.WaitGroup
   for i := 0; i < 100; i++ {
       wg.Add(1)
       go func(idx int) {
           defer wg.Done()
           ack, err := session.Produce(context.Background(), producerclient.CreateTaskRequest{
               Command: "echo",
               Payload: []byte(fmt.Sprintf(`{"message":"task %d"}`, idx)),
               Priority: 0,
           })
           if err != nil {
               log.Printf("Task %d failed: %v", idx, err)
               return
           }
           log.Printf("Task %d → ID %s", idx, ack.TaskID)
       }(i)
   }
   wg.Wait()
   ````

4. **Verify server received them:**
   Use `curl` to list pending tasks:
   ````bash
   curl -H "Authorization: Bearer $CODEQ_TOKEN" \
        http://localhost:9090/v1/codeq/tasks?status=pending | jq '.tasks | length'
   ````

**Takeaway:** Producer streaming pipelines requests across goroutines. `Produce()` returns the task ID as soon as the server's persistent layer acknowledges, allowing your code to dispatch many tasks before waiting for responses.

---

### Tutorial 2: Worker Streaming — Your First Processor

Learn to claim and process tasks via streaming. You'll set up a worker with multiple concurrent slots and watch it drain the queue.

**Goal:** Process tasks faster than REST allows, with independent concurrent slots running in parallel.

**Prerequisites:**
- codeQ server running on `localhost:9091`
- A valid bearer token with worker scope
- Some pending tasks (use producer streaming to create them)

**Steps:**

1. **Dial the server:**
   ````go
   import "github.com/osvaldoandrade/codeq/pkg/workerclient"
   
   client, err := workerclient.New(workerclient.Config{
   	Addr:        "localhost:9091",
   	Token:       os.Getenv("CODEQ_TOKEN"),
   	Concurrency: 5, // up to 5 tasks in flight
   })
   if err != nil {
       return err
   }
   defer client.Close()
   ````

2. **Define your handler:**
   ````go
   handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
       log.Printf("[%s] Processing: %s", task.ID, task.Command)
       
       // Simulate work
       time.Sleep(100 * time.Millisecond)
       
       // Return success
       return workerclient.Completed(map[string]any{
           "processed_at": time.Now().Format(time.RFC3339),
       })
   }
   ````

3. **Start the stream:**
   ````go
   if err := client.Run(context.Background(), handler); err != nil {
       log.Fatal(err)
   }
   ````

   The worker will now:
   - Open one stream to the server
   - Spawn 5 concurrent slots
   - Each slot independently: sends Ready → receives Task → calls handler → sends Result → repeats
   - If any slot fails, others continue running
   - To stop, cancel the context or close the client

4. **Observe slots working in parallel:**

   The handler is called concurrently. Slots run independently, so one slow task doesn't block others:

   ````
   [task-1] Processing: email-send
   [task-2] Processing: webhook-post
   [task-3] Processing: image-resize
   [task-1] Done: completed in 150ms
   [task-2] Done: failed after 120ms
   [task-4] Processing: data-export
   ...
   ````

**Takeaway:** Worker streaming maintains N independent Ready→Task→Result cycles. Each slot claims one task at a time but processes them in parallel. Configure `Concurrency` to match your handler's throughput and system resources.

---

## How-To Guides

### How to: Enable TLS/mTLS

By default, clients use insecure (plaintext) connections. For production, use TLS.

**Server-side TLS (server has cert, client doesn't need cert):**

````go
import "crypto/tls"
import "google.golang.org/grpc/credentials"

tlsCreds := credentials.NewClientTLSFromCert(nil, "codeq.example.com")
// nil means: use system root CAs

client, err := producerclient.New(producerclient.Config{
	Addr:        "codeq.example.com:9092",
	Token:       token,
	DialOptions: []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
	},
})
````

**Mutual TLS (mTLS, both sides have certs):**

````go
import "crypto/tls"
import "google.golang.org/grpc/credentials"

cert, err := tls.LoadX509KeyPair("client-cert.pem", "client-key.pem")
if err != nil {
	return err
}

tlsCreds := credentials.NewTLS(&tls.Config{
	Certificates: []tls.Certificate{cert},
	ServerName:   "codeq.example.com",
})

client, err := workerclient.New(workerclient.Config{
	Addr:        "codeq.example.com:9091",
	Token:       token,
	DialOptions: []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
	},
})
````

---

### How to: Handle Task Failures & Retries

Tasks can fail and be retried. Use `Failed()` to mark permanent failures; use `Nack()` to retry after a delay.

**Mark a task as permanently failed:**

````go
handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
	if task.Attempts >= task.MaxAttempts {
		// Too many retries; give up
		return workerclient.Failed("too many retries")
	}
	
	err := doWork(ctx, task.Payload)
	if err != nil {
		// Retry after 10 seconds
		return workerclient.Nack(10, err.Error())
	}
	
	return workerclient.Completed(nil)
}
````

**Graceful degradation — abandon if shutter down:**

````go
ctx, cancel := context.WithCancel(context.Background())
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, syscall.SIGTERM)

go func() {
	<-sigChan
	cancel()
}()

handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
	select {
	case <-ctx.Done():
		// Shutting down; release the task without nacking
		return workerclient.Abandon()
	default:
	}
	
	// Process task...
	return workerclient.Completed(nil)
}

if err := client.Run(ctx, handler); err != nil && err != context.Canceled {
	log.Printf("stream error: %v", err)
}
````

---

### How to: Monitor Streaming Performance

Streaming performance depends on message rate, payload size, and network latency. Monitor these metrics:

**Producer-side (submission rate):**

````go
import "sync/atomic"

var produced atomic.Int64
var startTime = time.Now()

for i := 0; i < 10000; i++ {
	ack, err := session.Produce(ctx, producerclient.CreateTaskRequest{...})
	if err == nil {
		produced.Add(1)
	}
}

elapsed := time.Since(startTime)
rate := float64(produced.Load()) / elapsed.Seconds()
log.Printf("Production rate: %.0f tasks/sec", rate)
````

**Worker-side (processing rate):**

````go
var processed atomic.Int64

handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
	defer func() { processed.Add(1) }()
	
	// ... do work ...
	
	return workerclient.Completed(nil)
}

go func() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		log.Printf("Processing rate: %.0f tasks/sec", float64(processed.Load()) / 10)
		processed.Store(0)
	}
}()

client.Run(ctx, handler)
````

**Expected throughput:**

- **Producer:** ~33,000 tasks/sec per stream on loopback (single goroutine).
- **Worker:** Depends on concurrency and handler latency. With `Concurrency=10` and 100ms per task: ~100 tasks/sec. Increase concurrency or reduce handler latency to scale higher.

---

### How to: Implement Idempotent Task Submission

Producer streaming supports optional `IdempotencyKey` for exactly-once task submission. If a network error occurs mid-submission, you can safely retry.

````go
import "github.com/google/uuid"

idempotencyKey := uuid.New().String()

ack, err := session.Produce(context.Background(), producerclient.CreateTaskRequest{
	Command:       "process-payment",
	Payload:       []byte(`{"amount":99.99}`),
	IdempotencyKey: idempotencyKey,
})
if err != nil {
	// Network error; safe to retry with same idempotency key
	// The server will return the same task ID
	log.Printf("Retry with key %s", idempotencyKey)
	ack, err = session.Produce(context.Background(), producerclient.CreateTaskRequest{
		Command:       "process-payment",
		Payload:       []byte(`{"amount":99.99}`),
		IdempotencyKey: idempotencyKey,
	})
}

log.Printf("Task: %s", ack.TaskID)
````

---

## Technical Reference

### Producer Streaming Protocol

**Connection lifecycle:**

1. Client opens bidirectional gRPC stream to `ProducerStream.Stream()`
2. Client sends `Hello{token}`
3. Server validates bearer token, resolves tenant, replies `HelloAck{tenant_id, subject}`
4. Client sends zero or more `CreateTask` messages
5. Server acks each with `CreateAck{seq, ok, task_id or error_message}`
6. Either side can close; client typically closes when done

**Message types:**

| Message | Direction | Purpose |
|---------|-----------|---------|
| `Hello{token}` | Client→Server | Authenticate and resolve tenant |
| `HelloAck{tenant_id, subject}` | Server→Client | Confirm auth; provide tenant context |
| `CreateTask{seq, command, payload, …}` | Client→Server | Submit a task |
| `CreateAck{seq, ok, task_id}` | Server→Client | Ack a single CreateTask |
| `ServerError{code, message}` | Server→Client | Report stream-level error |

**Key properties:**

- **Pipelined async:** Client can send many `CreateTask` messages before receiving acks. Acks may arrive out of order.
- **Seq correlation:** Each `CreateAck` echoes the `seq` from the corresponding `CreateTask`, allowing correlation.
- **One auth:** Token validated once at stream-open; all tasks inherit the resolved tenant.

**Example exchange (pseudo-code):**

````
Client → Server: Hello{token: "..."}
Server → Client: HelloAck{tenant_id: "acme", subject: "producer-1"}
Client → Server: CreateTask{seq: 1, command: "email", payload: "..."}
Client → Server: CreateTask{seq: 2, command: "webhook", payload: "..."}
Server → Client: CreateAck{seq: 1, ok: true, task_id: "task-123"}
Server → Client: CreateAck{seq: 2, ok: true, task_id: "task-124"}
````

---

### Worker Streaming Protocol

**Connection lifecycle:**

1. Client opens bidirectional gRPC stream to `WorkerStream.Stream()`
2. Client sends `Hello{token, worker_id}`
3. Server validates bearer token, resolves tenant, replies `HelloAck{worker_id, tenant_id}`
4. Client enters loop (repeated N times in parallel for Concurrency=N):
   - Sends `Ready{commands, lease_seconds}`
   - Receives `TaskAssignment{task}`
   - (Application processes task via handler)
   - Sends `Result{task_id, status, result_json or error}`
   - Optionally receives `ResultAck{ok, error_message}`
   - Loop repeats
5. Either side can close

**Message types:**

| Message | Direction | Purpose |
|---------|-----------|---------|
| `Hello{token, worker_id}` | Client→Server | Authenticate and identify worker |
| `HelloAck{worker_id, tenant_id}` | Server→Client | Confirm auth; provide tenant context |
| `Ready{commands, lease_seconds}` | Client→Server | Claim one task (block until available) |
| `TaskAssignment{task}` | Server→Client | Assign a task to this Ready |
| `Result{task_id, status, result_json/error}` | Client→Server | Submit completion |
| `ResultAck{ok, error_message}` | Server→Client | Ack the Result |
| `Nack{task_id, delay_seconds, reason}` | Client→Server | Return task to queue with delay |
| `NackAck{ok, …, dlq}` | Server→Client | Ack the Nack (dlq=true if MaxAttempts exceeded) |
| `Abandon{task_id}` | Client→Server | Release task without nacking |
| `AbandonAck{ok, …}` | Server→Client | Ack the Abandon |
| `Heartbeat{task_id, extend_seconds}` | Client→Server | Extend lease on long-running task |
| `HeartbeatAck{ok, …}` | Server→Client | Ack the heartbeat |

**Concurrency model:**

- With `Concurrency=5`, the client spawns 5 independent goroutines ("slots").
- Each slot independently: Ready → TaskAssignment → (handler) → Result → repeat.
- Slots run in parallel; one blocked/slow slot does not affect others.
- Each `Ready` is paired with exactly one `TaskAssignment` (1:1 pairing).

**Example exchange (single slot; pseudo-code):**

````
Client → Server: Hello{token: "...", worker_id: "w1"}
Server → Client: HelloAck{worker_id: "w1", tenant_id: "acme"}
Client → Server: Ready{commands: ["email", "webhook"]}
Server → Client: TaskAssignment{task: Task{id: "task-123", command: "email", ...}}
(application handler runs)
Client → Server: Result{task_id: "task-123", status: "COMPLETED", result_json: "{}"}
Server → Client: ResultAck{ok: true}
Client → Server: Ready{commands: ["email", "webhook"]}
Server → Client: TaskAssignment{task: Task{...}}
...
````

**Result semantics:**

- `Completed` — Task finished successfully; task is marked COMPLETED and removed from queue.
- `Failed` — Task failed; server increments attempts. If `attempts >= max_attempts`, task goes to DLQ. Otherwise, it returns to pending queue.
- `Nack{delay_seconds}` — Task failed temporarily; returns to pending queue after `delay_seconds` seconds.
- `Abandon` — Release the lease without changing queue state; task immediately re-claimable by another worker.

---

### Configuration Reference

#### ProducerClient Config

````go
type Config struct {
	// Addr is the gRPC dial target (e.g. "localhost:9092").
	// Required.
	Addr string

	// Token is the bearer token presented in Hello.
	// Required.
	Token string

	// DialOptions are forwarded to grpc.NewClient. If empty, uses insecure
	// transport. Set to TLS credentials for production.
	DialOptions []grpc.DialOption

	// Logger receives structured info/warn/error events.
	// Defaults to slog.Default().
	Logger *slog.Logger
}
````

#### WorkerClient Config

````go
type Config struct {
	// Addr is the gRPC dial target (e.g. "localhost:9091").
	// Required.
	Addr string

	// Token is the bearer token presented in Hello.
	// Required.
	Token string

	// WorkerID identifies this worker for lease ownership.
	// If empty, server uses the JWT subject from Token.
	WorkerID string

	// Commands restricts what this worker claims.
	// If empty/nil, worker pulls all available commands.
	Commands []string

	// Concurrency is the number of in-flight tasks.
	// Each slot runs Ready→Task→Result cycles in parallel.
	// Defaults to 1.
	Concurrency int

	// LeaseSeconds is sent on each Ready. 0 means "server default".
	LeaseSeconds int

	// IdleBackoff is the delay between Ready retries when no task arrived.
	// Defaults to 50ms.
	IdleBackoff time.Duration

	// DialOptions are forwarded to grpc.NewClient. If empty, uses insecure
	// transport. Set to TLS credentials for production.
	DialOptions []grpc.DialOption

	// Logger receives structured info/warn/error events.
	// Defaults to slog.Default().
	Logger *slog.Logger
}
````

---

### Error Handling

**Producer errors:**

- `Unauthenticated` — Token invalid or expired. Reconnect with new token.
- `PermissionDenied` — Token lacks producer scope. Check JWKS / token config.
- `InvalidArgument` — Malformed CreateTask (missing command, invalid priority, etc.). Fix request and retry.
- `ResourceExhausted` — Rate limited. Back off and retry with exponential delay.
- `Internal` — Server error. Log and retry or alert ops.

**Worker errors:**

- `Unauthenticated` — Token invalid or expired. Reconnect with new token.
- `PermissionDenied` — Token lacks worker scope. Check JWKS / token config.
- `NotFound` — Task ID not found (task completed or expired). Nack or Abandon not applicable; client should ignore.
- `FailedPrecondition` — Task not in INPROGRESS (already completed by another worker). Client should ignore.
- `DeadlineExceeded` — Lease expired while processing. Either extend via Heartbeat or Nack.
- `Internal` — Server error. Log and retry or alert ops.

---

## Explanation

### Why Streaming?

The REST API (`POST /tasks`, `POST /tasks/{id}/claim`, `POST /tasks/{id}/result`) requires one HTTP round-trip per operation. Each round-trip:
- Incurs auth overhead (token validation, tenant resolution)
- Pays connection setup cost (TLS handshake, TCP SYN-ACK)
- Serializes requests — one completes before the next starts

**Streaming amortizes these costs** across many messages on a single persistent connection:
- Auth happens once at stream-open
- Connection stays open for the lifetime of the producer/worker
- Messages pipeline asynchronously — many can be in flight simultaneously

**Result:** 2–3× throughput improvement on typical workloads.

---

### Throughput Characteristics

**Producer streaming:**

Single client on localhost:

- **Sequential (1 task at a time, waiting for ack):** ~5,000 tasks/sec (limited by RTT)
- **Pipelined (100 tasks in flight):** ~33,000 tasks/sec (limited by message rate)

Bottleneck: Network bandwidth and server processing. With larger payloads or higher latency, throughput decreases. Test on your target infrastructure.

**Worker streaming:**

Single client with `Concurrency=C`, handler latency `L` ms:

- **Throughput ≈ (C / L) × 1000 tasks/sec**

Examples:
- `Concurrency=10`, `L=100ms`: ~100 tasks/sec
- `Concurrency=50`, `L=50ms`: ~1,000 tasks/sec
- `Concurrency=100`, `L=10ms`: ~10,000 tasks/sec

Bottleneck: Handler processing speed and system resources. Increase concurrency and parallelize handler logic to scale.

---

### Performance Optimizations

**Producer side:**

1. **Pipeline many tasks:** Use goroutines to send many `CreateTask` messages before awaiting acks. The server processes them concurrently.

   ````go
   var wg sync.WaitGroup
   for i := 0; i < 1000; i++ {
       wg.Add(1)
       go func(idx int) {
           defer wg.Done()
           session.Produce(ctx, createTaskRequest)
       }(i)
   }
   wg.Wait()
   ````

2. **Batch operations:** Group related tasks and submit them together. Reduces latency variance.

3. **Reuse sessions:** One session per producer. Avoid opening/closing repeatedly.

**Worker side:**

1. **Configure concurrency:** Match `Concurrency` to system resources and handler throughput.
   - Start with `Concurrency=CPU_cores`, then tune based on profiling.
   - If handlers are mostly blocking I/O (HTTP, database), increase concurrency (e.g., 50–100).
   - If handlers are CPU-intensive, cap at CPU cores.

2. **Minimize handler latency:** Move blocking operations outside the hot path.
   - Pre-connect to databases, caches.
   - Use timeouts to avoid infinite hangs.

3. **Lease tuning:** Adjust `LeaseSeconds` based on expected handler duration.
   - Too short: Tasks timeout mid-processing and get re-claimed.
   - Too long: Failed workers hold leases for extended periods.
   - Suggested: 3–10× expected handler latency.

---

### Streaming vs. REST Comparison

| Aspect | Streaming | REST |
|--------|-----------|------|
| **Auth overhead** | Once per stream | Per request |
| **Connection cost** | Once per stream | Per request |
| **Throughput** | 2–3× higher | 1× baseline |
| **Latency tail** | Lower (fewer RTTs) | Higher (one RTT per op) |
| **Complexity** | Slightly higher (proto, goroutines) | Lower (simple HTTP) |
| **Debugging** | Requires gRPC tools | `curl` friendly |
| **Ideal for** | High-frequency producers/workers | Ad-hoc, low-frequency ops |

**When to use streaming:**

- High-frequency task submission (>100 tasks/sec)
- Many workers competing for tasks
- Latency-sensitive workflows
- Batch processing at scale

**When to use REST:**

- Ad-hoc task submission
- Low frequency (<10 tasks/sec)
- Simpler operational setup
- Integration with existing HTTP tooling

---

## Appendix: Code Examples

### Full Producer Example

````go
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
	addr := flag.String("addr", "localhost:9092", "gRPC server address")
	token := flag.String("token", os.Getenv("CODEQ_TOKEN"), "bearer token")
	count := flag.Int("count", 100, "number of tasks to produce")
	parallelism := flag.Int("parallelism", 10, "concurrent goroutines")
	flag.Parse()

	client, err := producerclient.New(producerclient.Config{
		Addr:  *addr,
		Token: *token,
	})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	session, err := client.Connect(context.Background())
	if err != nil {
		log.Fatalf("connect: %v", err)
	}

	var produced atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, *parallelism)

	for i := 0; i < *count; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			ack, err := session.Produce(context.Background(), producerclient.CreateTaskRequest{
				Command: "demo-task",
				Payload: []byte(`{"index":` + string(rune(idx)) + `}`),
			})
			if err != nil {
				log.Printf("task %d failed: %v", idx, err)
				return
			}
			produced.Add(1)
			log.Printf("Task %d → %s", idx, ack.TaskID)
		}(i)
	}

	wg.Wait()
	log.Printf("Produced %d tasks", produced.Load())
}
````

### Full Worker Example

````go
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
	addr := flag.String("addr", "localhost:9091", "gRPC server address")
	token := flag.String("token", os.Getenv("CODEQ_TOKEN"), "bearer token")
	concurrency := flag.Int("concurrency", 5, "concurrent slots")
	flag.Parse()

	client, err := workerclient.New(workerclient.Config{
		Addr:        *addr,
		Token:       *token,
		Concurrency: *concurrency,
	})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	var processed atomic.Int64

	handler := func(ctx context.Context, task workerclient.Task) workerclient.Result {
		defer processed.Add(1)
		defer log.Printf("Completed: %s", task.ID)

		log.Printf("Processing: %s (attempts: %d/%d)", task.ID, task.Attempts, task.MaxAttempts)

		// Simulate work
		time.Sleep(100 * time.Millisecond)

		return workerclient.Completed(map[string]any{
			"processed_at": time.Now().Format(time.RFC3339),
		})
	}

	// Print stats periodically
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			log.Printf("Processed so far: %d", processed.Load())
		}
	}()

	if err := client.Run(context.Background(), handler); err != nil {
		log.Fatalf("run: %v", err)
	}
}
````

---

## See Also

- [Architecture Guide](03-architecture.md) — gRPC Streaming Flows section
- [HTTP API Reference](04-http-api.md) — REST endpoints (baseline for comparison)
- [Performance Tuning](17-performance-tuning.md) — System-level optimization
- [Package Reference](18-package-reference.md) — Go SDK API docs
