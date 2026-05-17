# Streaming API Guide

## Overview

The codeQ streaming APIs provide high-performance gRPC-based alternatives to the REST APIs for producer task submission and worker task claiming. Streaming achieves **2–3× throughput improvement** compared to REST by:

- Amortizing authentication and tenant resolution over the stream lifetime (not per-request)
- Enabling async pipelining: multiple requests in flight before the first response
- Eliminating per-call HTTP middleware overhead

**Phase 3 throughput optimization** replaces the REST create path (`POST /tasks`) with producer streaming; **Phase 2** replaces the worker claim-and-result loop with worker streaming. Production deployments should migrate from REST to streaming APIs.

---

## Producer Streaming API

### High-Level Flow

1. **Client opens bidirectional gRPC stream** to the producer stream endpoint (default port 9092)
2. **Client sends `Hello` with bearer token** → server validates, resolves tenant, replies `HelloAck`
3. **Client sends `CreateTask` events with monotonically-increasing `seq` numbers**
4. **Server acks each `CreateTask` with `CreateAck` (echoing `seq` and `task_id` or error)**
5. **Multiple goroutines can call `Produce()` concurrently** — each blocks until the matching `CreateAck` arrives

### Tutorial: Async Task Production

Start a producer stream and submit tasks with full pipelining:

````go
package main

import (
	"context"
	"log"

	"github.com/osvaldoandrade/codeq/pkg/producerclient"
)

func main() {
	cfg := producerclient.Config{
		Addr:  "localhost:9092",
		Token: "your-producer-token",
	}

	session, err := producerclient.Connect(context.Background(), cfg)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer session.Close()

	// Submit multiple tasks concurrently; each Produce blocks only until
	// the corresponding CreateAck arrives. The client auto-correlates via seq.
	for i := 0; i < 100; i++ {
		go func(taskNum int) {
			taskID, err := session.Produce(context.Background(),
				producerclient.CreateTaskRequest{
					Command:   "GENERATE_REPORT",
					Payload:   []byte(`{"reportID":"r-123"}`),
					Priority:  5,
				},
			)
			if err != nil {
				log.Printf("produce task %d failed: %v", taskNum, err)
			} else {
				log.Printf("task %d assigned ID %s", taskNum, taskID)
			}
		}(i)
	}

	// Wait for all acks (or timeout).
	session.Wait(context.Background())
}
````

### Protocol Reference

**Client→Server:**

- **`Hello`**: bearer token, sent once at stream open
- **`CreateTask`** (repeated): task submission with monotonically-increasing `seq`
  - `seq`: producer-assigned, must strictly increase per stream
  - `command`: task type (e.g., "GENERATE_REPORT")
  - `payload`: opaque JSON bytes (stored as-is server-side)
  - `priority`: 0–10 (optional, default 5)
  - `webhook`: result callback URL (optional)
  - `max_attempts`: retry limit (optional, default 3)
  - `idempotency_key`: deduplication key (optional)
  - `run_at`: RFC3339 scheduled start time (optional, mutually exclusive with `delay_seconds`)
  - `delay_seconds`: seconds before task is claimable (optional, mutually exclusive with `run_at`)
  - `trace_parent` / `trace_state`: W3C trace context (optional)

**Server→Client:**

- **`HelloAck`**: tenant ID and JWT subject, sent once per stream
- **`CreateAck`**: response to a single `CreateTask`
  - `seq`: echoes the request `seq` for correlation
  - `ok`: true if create succeeded
  - `task_id`: server-assigned task ID (if `ok=true`)
  - `error_message`: human-readable error (if `ok=false`)
- **`ServerError`**: stream-level error (closes stream)

### How-To: TLS/mTLS Transport

Pass TLS credentials via `DialOptions`:

````go
import "google.golang.org/grpc/credentials"

cfg := producerclient.Config{
	Addr:  "codeq.example.com:9092",
	Token: "your-producer-token",
	DialOptions: []grpc.DialOption{
		grpc.WithTransportCredentials(
			credentials.NewClientTLSFromCert(
				certPool, "codeq.example.com",
			),
		),
	},
}
````

### Error Handling

**`CreateAck` with `ok=false`:**

- **`idempotency_key_conflict`**: A task with this key was already created. Retry is safe (idempotent).
- **`validation_error`**: Payload or fields were invalid. Retry will fail with the same error; fix and resubmit.
- **Other errors**: Infrastructure error. Retry with exponential backoff.

**Stream-level errors:**

- **`Unauthenticated`** (code 16): Bearer token is invalid or expired. Reconnect with a new token.
- **`PermissionDenied`** (code 7): Token lacks task-creation permissions. Check scopes.
- **`FailedPrecondition`** (code 9): Server is not ready (e.g., cluster forming). Retry with backoff.

### Performance Notes

- **Concurrency**: Goroutines calling `Produce()` in parallel are safe and recommended.
- **Batching**: No explicit batching needed; pipelining is automatic. `Produce()` unblocks as soon as the individual `CreateAck` arrives.
- **Throughput**: ~33k tasks/sec per stream on a single core; scale by opening multiple streams or goroutines.

---

## Worker Streaming API

### High-Level Flow

1. **Client opens bidirectional gRPC stream** to the worker stream endpoint (default port 9091)
2. **Client sends `Hello` with bearer token** → server validates, resolves tenant, replies `HelloAck`
3. **Client spawns N concurrent slots** (configured by `Concurrency`; default 1)
4. **Each slot independently loops:**
   - Send `Ready` with desired commands and lease duration
   - Server sends `Task` when available
   - Handler processes task (user callback)
   - Send `Result` (one of: `Completed`, `Failed`, `Nack`, `Abandon`)
   - Back to Ready
5. **Slots run in parallel; one task failure doesn't block siblings**

### Tutorial: Worker with Concurrent Slots

Start a worker that claims and processes up to 10 tasks in parallel:

````go
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"

	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

func main() {
	cfg := workerclient.Config{
		Addr:        "localhost:9091",
		Token:       "your-worker-token",
		WorkerID:    "worker-1",
		Commands:    []string{"GENERATE_REPORT", "SEND_EMAIL"},
		Concurrency: 10,
		LeaseSeconds: 300,
		Logger:      slog.Default(),
	}

	client, err := workerclient.NewClient(cfg)
	if err != nil {
		log.Fatalf("new client failed: %v", err)
	}

	err = client.Run(context.Background(), handler)
	if err != nil {
		log.Fatalf("run failed: %v", err)
	}
}

// handler is called concurrently (up to Concurrency times).
// It must be safe for concurrent invocation.
func handler(ctx context.Context, task workerclient.Task) workerclient.Result {
	log.Printf("processing task %s (command=%s)", task.ID, task.Command)

	var payload map[string]any
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return workerclient.Failed("invalid JSON payload: " + err.Error())
	}

	// Example: GENERATE_REPORT task
	if task.Command == "GENERATE_REPORT" {
		reportID, ok := payload["reportID"].(string)
		if !ok {
			return workerclient.Failed("missing reportID in payload")
		}

		// Simulate work
		result := map[string]any{"report_url": "s3://reports/" + reportID}
		return workerclient.Completed(result)
	}

	return workerclient.Failed("unknown command: " + task.Command)
}
````

### Protocol Reference

**Client→Server:**

- **`Hello`**: bearer token and optional `worker_id`, sent once at stream open
- **`Ready`** (repeated): "I have capacity; send me a task"
  - `commands`: filter to these command types (optional; uses token scopes if absent)
  - `lease_seconds`: how long to hold the task before auto-requeue (0 means server default)
- **`Result`**: task completion (one per claimed task)
  - `task_id`: the task being completed
  - `status`: one of `COMPLETED` or `FAILED`
  - `result_json`: result payload (present for `COMPLETED`)
  - `error`: error message (present for `FAILED`)
- **`Nack`**: return task to queue with delay
  - `task_id`: the task to requeue
  - `delay_seconds`: when to re-claim (0 = immediately)
  - `reason`: human-readable requeue reason
- **`Heartbeat`**: extend lease on in-progress task
  - `task_id`: the task
  - `extend_seconds`: additional time
- **`Abandon`**: release lease without requeue (goes straight to `PENDING`)
  - `task_id`: the task

**Server→Client:**

- **`HelloAck`**: worker ID and tenant ID, sent once per stream
- **`TaskAssignment`**: one task claimed (sent in response to each `Ready`)
  - `task`: the claimed task (mirrors REST task model)
- **`ResultAck`**: response to `Result`
  - `ok`: true if result was accepted
  - `error_message`: reason if `ok=false` (e.g., "not-found", "not-owner", "not-in-progress")
- **`NackAck`**: response to `Nack`
  - `ok`: true if requeue was accepted
  - `applied_delay_seconds`: actual delay applied by server
  - `dlq`: true if task was sent to dead-letter queue (max retries exceeded)
- **`HeartbeatAck`** / **`AbandonAck`**: responses to heartbeat/abandon
- **`ServerError`**: stream-level error (closes stream)

### How-To: Stateful Slot Lifecycle

Each slot maintains independent state across the Ready→Task→Result loop:

````go
// Pseudo-code showing slot lifecycle
for {
	// Send Ready
	stream.Send(&Ready{
		commands:     ["GENERATE_REPORT"],
		lease_seconds: 300,
	})

	// Receive Task
	task := stream.Recv() // blocks until task available

	// Run handler
	result := handler(ctx, task)

	// Send Result / Nack / Heartbeat / Abandon
	if err := stream.Send(result); err != nil {
		// Slot error; slot exits, other slots continue
		break
	}

	// Back to Ready
}
````

### How-To: Graceful Shutdown

On shutdown, `Abandon` in-flight tasks so they return to the queue immediately:

````go
func shutdown(client *workerclient.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Close stream and abandon all in-flight tasks
	client.Close(ctx)
}
````

### Error Handling

**`ResultAck` with `ok=false`:**

- **`not-found`**: Task no longer exists (may have timed out or been claimed elsewhere). Safe to continue.
- **`not-owner`**: You don't hold the lease. Another worker claimed it. Safe to continue.
- **`not-in-progress`**: Task already completed or failed. Safe to continue.

**`NackAck` with `dlq=true`:**

Task exceeded `max_attempts`. It's been moved to the dead-letter queue and won't be retried. Log for manual investigation.

**Stream-level errors:**

- **`Unauthenticated`** (code 16): Bearer token is invalid or expired. Reconnect with a new token.
- **`PermissionDenied`** (code 7): Token lacks task-claim permissions or the requested commands are outside your scope. Check token scopes.
- **`FailedPrecondition`** (code 9): Server is not ready (e.g., cluster forming). Reconnect with backoff.

### Performance Notes

- **Concurrency**: Each slot holds one task end-to-end. If your handler takes 1 second and you want 10 tasks in flight, set `Concurrency: 10`.
- **Slots are independent**: A failure or delay in one slot doesn't block others. One task timeout won't slow down the other 9 slots.
- **Backoff on idle**: When no tasks are available, slots send `Ready` at intervals (default 100ms). Tune `IdleBackoff` in config to reduce CPU on idle workers.
- **Throughput**: ~33k task claims/sec per worker with default settings; increases linearly with `Concurrency`.

---

## Result Types: Disposition Decisions

A worker's result handler must return exactly one of these:

### `Completed(body map[string]any)`

Task succeeded. The result payload is JSON-encoded and stored. If `body` is nil, no payload is stored.

````go
return workerclient.Completed(map[string]any{
	"status": "done",
	"url": "s3://results/report-123.pdf",
})
````

Triggers: webhook callback (if configured), result becomes available for REST `GET /tasks/:id/result`.

### `Failed(err string)`

Task failed permanently. If `attempts < max_attempts`, the server will reschedule; otherwise, it goes to the DLQ.

````go
return workerclient.Failed("unable to connect to database: " + err.Error())
````

Triggers: webhook callback with error, then requeue (or DLQ if limit reached).

### `Nack(delaySeconds int, reason string)`

Task is not ready yet. Return it to the queue to be claimed again after `delaySeconds`.

````go
return workerclient.Nack(60, "upstream service unavailable, retry in 1m")
````

Use this for transient issues. The task keeps its attempt count and can exceed `max_attempts` indefinitely if you keep nacking. If `delaySeconds < 0`, it's clamped to 0 (immediate requeue).

### `Abandon()`

Release the lease without requeuing. Task goes straight back to `PENDING` and is immediately claimable by another worker. Used during graceful shutdown to hand off in-flight work.

````go
if len(shutdownCh) > 0 {
	return workerclient.Abandon()
}
````

---

## Comparison: REST vs. Streaming

| Aspect | REST (`POST /tasks`, `POST /claim`) | Streaming (gRPC) |
|--------|--------------------------------------|-----------------|
| **Auth overhead** | Per-request (JWKS fetch, validation) | Once per stream open |
| **Concurrency** | Sequential (response required before next request) | Pipelined (multiple in-flight) |
| **Throughput** | ~2k–5k ops/sec per core | ~10k–33k ops/sec per core |
| **Latency (p50)** | 5–15ms | 1–5ms |
| **Latency (p99)** | 50–200ms | 10–30ms |
| **Best for** | Simple one-off tasks, low-frequency workers | High-throughput producers/workers |
| **Prototyping** | Yes (simpler API surface) | Yes (more involved setup) |

---

## Explanation: Why Streaming is Faster

### 1. Connection Amortization

Each REST call incurs:
- TCP handshake (varies, often avoided via keep-alive)
- TLS handshake (rarely avoided; ~10ms round-trip)
- HTTP header encoding/decoding (~1ms)
- JWKS token validation (varies; 5–50ms on first call, cached thereafter)

A gRPC stream does all this once; subsequent messages are just protocol buffer serialization (~0.1–1ms per message).

### 2. Pipelining

REST is request-response: you must wait for the ack before sending the next request.

Streaming is fully async: you can send 100 tasks before the first ack arrives. The server processes them concurrently in the backend, and acks (or task assignments) come back in any order. The seq / correlation ID lets you match them up.

**Result:** REST saturates at ~2k requests/sec on a single core (network RTT limited); streaming on the same core can hit 10k–30k requests/sec (CPU limited).

### 3. No Middleware Tax per Call

REST calls traverse:
- HTTP routing (port, path matching)
- Authentication middleware (JWKS, tenant extraction, scopes)
- Rate limiting middleware (token bucket check)
- Logging middleware
- Error handling middleware

Each middleware frame-and-unframe a request body. Streaming does this once at stream open.

---

## Migration Guide: REST → Streaming

### Producer: `POST /tasks` → Streaming

**Before (REST):**

````go
for i := 0; i < 100; i++ {
	resp, err := http.Post("http://localhost:8080/v1/codeq/tasks",
		"application/json",
		bytes.NewReader(taskJSON),
	)
	// wait for resp before submitting next task
}
````

**After (Streaming):**

````go
session, _ := producerclient.Connect(ctx, cfg)
defer session.Close()

for i := 0; i < 100; i++ {
	go func(i int) {
		taskID, _ := session.Produce(ctx, createReq)
		// returns immediately, ack arrives asynchronously
	}(i)
}

session.Wait(ctx) // wait for all acks
````

### Worker: Loop of `POST /claim` + `POST /result` → Streaming

**Before (REST):**

````go
for {
	// Claim one task
	task, _ := http.Post("http://localhost:8080/v1/codeq/tasks/claim", ...)

	// Process
	result := handler(task)

	// Submit result
	http.Post(fmt.Sprintf("http://localhost:8080/v1/codeq/tasks/%s/result", task.ID), ...)
}
````

**After (Streaming):**

````go
cfg := workerclient.Config{
	Addr:        "localhost:9091",
	Token:       token,
	Concurrency: 10,
}

client, _ := workerclient.NewClient(cfg)
client.Run(ctx, handler) // concurrency automatic
````

---

## Troubleshooting

### Stream Hangs on First Message

**Symptom:** `Connect()` or `Run()` blocks indefinitely on `Hello`.

**Cause:** Server is not listening on the streaming port (9091/9092) or firewall blocks it.

**Solution:** Check `gRPC.Addr` in server config, verify port is open, test with `grpcurl`:

````bash
grpcurl -plaintext localhost:9092 list
````

### "Unauthenticated" Error

**Symptom:** Stream opens but immediately closes with "Unauthenticated".

**Cause:** Bearer token is invalid, expired, or has wrong scope.

**Solution:** Verify token is valid for the intended flow:
- Producer token: must have `codeq:create` scope
- Worker token: must have `codeq:claim` and `codeq:result` scopes

### Results Not Accepted ("not-owner")

**Symptom:** `ResultAck` with `error_message: "not-owner"`.

**Cause:** Lease has expired or another worker claimed the same task.

**Solution:** Increase `LeaseSeconds` in worker config or process tasks faster. Monitor `lease_until` field in the `Task`.

### Goroutines Leak After Close

**Symptom:** Memory grows after `session.Close()` or `client.Close()`.

**Cause:** Internal goroutines not properly drained.

**Solution:** Always call `Close()` or `Close(ctx)` before exiting. Use a timeout to avoid indefinite waits.

---

## Deployment Checklist

- [ ] **Enable gRPC streaming endpoints** in server config (`gRPC.Addr` and `gRPC.Port`)
- [ ] **Open ports 9091 (worker) and 9092 (producer)** on your infrastructure
- [ ] **Issue tokens with correct scopes**: `codeq:create` for producers, `codeq:claim` + `codeq:result` for workers
- [ ] **Set `Concurrency` on workers** based on expected load and handler latency (e.g., 10 for 100ms handler)
- [ ] **Monitor stream health**: track active stream count, message latency, error rates
- [ ] **Migrate gradually**: run REST and streaming APIs in parallel during transition; cut over REST once streaming proves stable
- [ ] **Tune backoff parameters** (`IdleBackoff`, `ReadyTimeout`) based on your workload

---

## See Also

- [HTTP API Guide](04-http-api.md) — REST alternatives
- [Architecture Overview](03-architecture.md) — System design and gRPC integration
- [Performance Tuning](17-performance-tuning.md) — Streaming benchmarks and configuration
- [Security Guide](09-security.md) — Token scopes and TLS/mTLS setup
- [Examples](13-examples.md) — Complete working samples
