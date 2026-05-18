# Usage examples

Short, copy-pasteable recipes for the three ways to talk to a codeq
server: raw HTTP (`curl`), the `codeq` CLI, and the Go SDK. For the
full walkthrough that builds a runnable producer + worker service, see
[Go SDK tutorial](./44-tutorial-go-sdk.md).

## Table of contents

- [HTTP API examples](#http-api-examples)
- [CLI examples](#cli-examples)
- [Go SDK examples](#go-sdk-examples)
- [OpenTelemetry distributed tracing](#opentelemetry-distributed-tracing)
- [Additional resources](#additional-resources)

## HTTP API examples

The HTTP API is for one-off calls and non-Go clients. Long-running
producers and workers should prefer the gRPC streaming SDKs
(`pkg/producerclient`, `pkg/workerclient`) because each HTTP request
pays the full middleware tax (TLS handshake amortised by keep-alive,
JSON parse, auth, request logging) — the streaming path skips all of
that after the initial Hello handshake.

### Producer: create task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

### Producer: create scheduled task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"runAt":"2026-01-25T13:10:00Z"}'
```

### Worker: claim task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

### Worker: submit result

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/result \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"COMPLETED","result":{"ok":true}}'
```

### Worker: heartbeat

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/heartbeat \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"leaseSeconds":120}'
```

### Worker: NACK (retry later)

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/nack \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"reason":"temporary_failure"}'
```

### Worker: abandon task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/abandon \
  -H 'Authorization: Bearer <worker-token>'
```

### Worker: register webhook

```bash
curl -X POST http://localhost:8080/v1/codeq/workers/subscriptions \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"eventTypes":["GENERATE_MASTER"],"callbackUrl":"https://worker.example.org/codeq/notify","ttlSeconds":300}'
```

### Admin: queue statistics

```bash
curl -X GET http://localhost:8080/v1/codeq/admin/queue/stats \
  -H 'Authorization: Bearer <admin-token>'
```

## CLI examples

The CLI is shipped as a separate binary in `cmd/codeq` and primarily
calls the HTTP API. See [CLI Reference](./15-cli-reference.md) for the
full flag list and profile configuration.

### Start a local server

The server binary is `cmd/server`, not a `codeq` subcommand. Build and
run it directly:

```bash
go build -o bin/codeq-server ./cmd/server
./bin/codeq-server -config deploy/config/codeq.example.yml
```

For a working dev setup including Pebble shards and Prometheus, see
[Getting started](./00-getting-started.md).

### Generate an install bundle

```bash
codeq install --target docker --size dev --no-prompt
```

This emits a `codeq-install/` directory with a Docker Compose stack
and a prebuilt image tag. The Kubernetes target (`--target k8s`) emits
a Helm chart.

### Create a task

```bash
codeq task create \
  --event PROCESS_ORDER \
  --payload '{"orderId":"123"}' \
  --priority 5
```

The token is sourced from `--producer-token`, `CODEQ_PRODUCER_TOKEN`,
or the current profile in `~/.codeq/config.yaml`.

### Fetch a task and its result

```bash
codeq task get 01HWXYZ1234567890ABCDEFGH
codeq task result 01HWXYZ1234567890ABCDEFGH
```

### Inspect queue state

```bash
codeq queue stats
codeq worker inspect PROCESS_ORDER
```

## Go SDK examples

The Go SDK lives inside the main module under
[`pkg/producerclient`](../pkg/producerclient/client.go) (task creation)
and [`pkg/workerclient`](../pkg/workerclient/client.go) (claim and
result). Both open one long-lived bidirectional gRPC stream after a
single Hello handshake. The producer stream listens on `:9092`, the
worker stream on `:9091` (defaults — overridable via config).

For a runnable end-to-end walkthrough that builds both halves of a
service, see [Go SDK tutorial](./44-tutorial-go-sdk.md).

**Install:**

```bash
go get github.com/osvaldoandrade/codeq
```

**Create one task (producer):**

```go
import (
    "context"
    "os"

    "github.com/osvaldoandrade/codeq/pkg/producerclient"
)

cli, err := producerclient.New(producerclient.Config{
    Addr:  "codeq.example.com:9092",
    Token: os.Getenv("CODEQ_PRODUCER_TOKEN"),
})
if err != nil {
    return err
}
defer cli.Close()

sess, err := cli.Connect(context.Background())
if err != nil {
    return err
}
defer sess.Close()

taskID, err := sess.Produce(context.Background(), producerclient.CreateRequest{
    Command:  "PROCESS_ORDER",
    Payload:  []byte(`{"orderId":"123","amount":99.99}`),
    Priority: 5,
})
```

`Produce` blocks only until the server's `CreateAck` arrives. Many
goroutines can call `Produce` on the same `Session` concurrently — the
client multiplexes them via sequence numbers
(`pkg/producerclient/client.go:104-131`).

**Produce a batch in one stream frame:**

```go
reqs := []producerclient.CreateRequest{
    {Command: "PROCESS_ORDER", Payload: []byte(`{"orderId":"1"}`)},
    {Command: "PROCESS_ORDER", Payload: []byte(`{"orderId":"2"}`)},
    {Command: "PROCESS_ORDER", Payload: []byte(`{"orderId":"3"}`)},
}
results, err := sess.ProduceBatch(context.Background(), reqs)
if err != nil {
    return err
}
for i, r := range results {
    if r.Err != nil {
        log.Printf("task %d rejected: %v", i, r.Err)
        continue
    }
    log.Printf("task %d id=%s", i, r.TaskID)
}
```

`ProduceBatch` sends one `CreateTaskBatch` event instead of N
`CreateTask`, and the server returns one `CreateAckBatch`. The
per-task latency is not serialised — the server still fans out
internally. This is the path that gets the producer harness to 136k
creates/s (`internal/bench/producer_stream_vs_rest_test.go::TestProducerThroughput_StreamBatchPath`).

**Process tasks (worker):**

`Client.Run` opens a stream, dispatches claimed tasks to a `Handler`,
and returns when the context is cancelled. The handler decides how to
finalize each task with `Completed`, `Failed`, `Nack`, or `Abandon`.

```go
import (
    "context"
    "os"

    "github.com/osvaldoandrade/codeq/pkg/workerclient"
)

w, err := workerclient.New(workerclient.Config{
    Addr:         "codeq.example.com:9091",
    Token:        os.Getenv("CODEQ_WORKER_TOKEN"),
    Commands:     []string{"PROCESS_ORDER"},
    Concurrency:  4,
    LeaseSeconds: 120,
    BatchSize:    8,
})
if err != nil {
    return err
}
defer w.Close()

handler := func(ctx context.Context, t workerclient.Task) workerclient.Result {
    if err := processOrder(ctx, t.Payload); err != nil {
        return workerclient.Nack(5, err.Error())
    }
    return workerclient.Completed(map[string]any{"success": true})
}

if err := w.Run(ctx, handler); err != nil {
    return err
}
```

**Result dispositions** (`pkg/workerclient/result.go`):

```go
workerclient.Completed(map[string]any{"ok": true})  // terminal success
workerclient.Failed("invalid payload: " + err.Error()) // terminal failure
workerclient.Nack(5, "downstream 503")              // retry after 5s, attempts++
workerclient.Abandon()                              // release lease, no attempt++
```

The single-node full-cycle harness sustains 83,420 tasks/s with this
SDK against an embedded Pebble store
(`internal/bench/profile_full_cycle_test.go::TestProfile_FullCycle`,
4 shards, 32 producer slots at batch=8, 128 worker slots, 12-core
Linux). See [`docs/_STYLE.md` § 7](./_STYLE.md#7-numbers-must-come-from-measurement)
for the canonical bench table.

**See also**:
- [Go SDK tutorial](./44-tutorial-go-sdk.md) — full walkthrough.
- [Producer streaming SDK](./35-producer-streaming-sdk.md) — wire
  protocol and Hello handshake.
- [Worker streaming SDK](./36-worker-streaming-sdk.md) — slot loop,
  batching, lease semantics.

For non-Go callers, use the HTTP API documented in
[HTTP API](./04-http-api.md).

## OpenTelemetry distributed tracing

### Basic configuration

Enable tracing in your configuration file or via environment variables:

````yaml
# config.yaml
tracingEnabled: true
tracingServiceName: codeq
tracingOtlpEndpoint: localhost:4317
tracingOtlpInsecure: true
tracingSampleRatio: 1.0
````

Or via environment:

````bash
export TRACING_ENABLED=true
export TRACING_SERVICE_NAME=codeq
export TRACING_OTLP_ENDPOINT=localhost:4317
export TRACING_OTLP_INSECURE=true
export TRACING_SAMPLE_RATIO=1.0
````

### Trace context propagation

When creating tasks, you can propagate trace context from your application:

````bash
# Include W3C trace context headers in your request
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" \
  -d '{
    "command": "PROCESS_ORDER",
    "payload": {"orderId": "12345"}
  }'
````

The trace context is:
- Extracted from incoming HTTP headers
- Stored with the task record
- Propagated to webhooks and result callbacks
- Included in all spans emitted by codeQ

### End-to-end tracing with Jaeger

````bash
# Start codeQ with observability stack
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  --profile obs up -d

# Enable tracing in .env
cat >> .env << 'EOF'
TRACING_ENABLED=true
TRACING_SERVICE_NAME=codeq
TRACING_OTLP_ENDPOINT=jaeger:4317
TRACING_OTLP_INSECURE=true
TRACING_SAMPLE_RATIO=1.0
EOF

# Restart codeQ
docker compose \
  -f deploy/docker-compose/local-dev/compose.yaml \
  -f deploy/docker-compose/local-dev/compose.override.yaml \
  restart codeq

# Create a task
TASK_ID=$(curl -s -X POST http://localhost:8080/v1/codeq/tasks \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"command":"EXAMPLE_TASK","payload":{"test":true}}' | jq -r '.id')

# Process the task
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"commands":["EXAMPLE_TASK"],"leaseSeconds":60}'

# Complete the task
curl -X POST http://localhost:8080/v1/codeq/tasks/$TASK_ID/result \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"status":"COMPLETED","output":{"success":true}}'

# View the trace in Jaeger UI
open http://localhost:16686
````

### Tracing inside a custom worker

If you're building a worker service in your application that processes codeQ tasks, ensure you:

1. **Extract trace context** from the task record (uses `traceParent` and `traceState` fields)
2. **Create child spans** for your processing logic
3. **Use the same service name** or a related one for correlation

Example in Go:

````go
import (
    "context"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/propagation"
)

// Extract trace context from task
func processTask(task *Task) {
    carrier := propagation.MapCarrier{}
    if task.TraceParent != "" {
        carrier.Set("traceparent", task.TraceParent)
    }
    if task.TraceState != "" {
        carrier.Set("tracestate", task.TraceState)
    }
    
    // Extract and create child span
    ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
    tracer := otel.Tracer("my-worker-service")
    ctx, span := tracer.Start(ctx, "process_task")
    defer span.End()
    
    // Your processing logic here
    // ...
}
````

### Sampling

Control what percentage of traces are sampled:

````yaml
tracingSampleRatio: 0.1  # Sample 10% of requests
````

Sampling is parent-based by default, so if an incoming request has a sampled trace context, codeQ will honor it.

## Additional resources

- **Go SDK tutorial**: [44-tutorial-go-sdk.md](./44-tutorial-go-sdk.md)
- **HTTP API reference**: [04-http-api.md](./04-http-api.md)
- **CLI reference**: [15-cli-reference.md](./15-cli-reference.md)
- **Producer streaming SDK**: [35-producer-streaming-sdk.md](./35-producer-streaming-sdk.md)
- **Worker streaming SDK**: [36-worker-streaming-sdk.md](./36-worker-streaming-sdk.md)
- **Tracing configuration**: [14-configuration.md § tracing](./14-configuration.md#tracing-opentelemetry)
- **Operations guide**: [10-operations.md § tracing](./10-operations.md#tracing-opentelemetry)
