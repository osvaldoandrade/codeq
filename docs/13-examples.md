# Usage Examples

This document provides practical examples of using CodeQ via HTTP API, CLI, and the official Go SDK.

## Table of Contents

- [HTTP API Examples](#http-api-examples)
- [CLI Examples](#cli-examples)
- [Go SDK Examples](#go-sdk-examples)

## HTTP API Examples

### Producer: create task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

## Producer: create scheduled task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"runAt":"2026-01-25T13:10:00Z"}'
```

## Worker: claim task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

## Worker: submit result

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

## CLI Examples

See [CLI Reference](15-cli-reference.md) for complete documentation.

### Start local server

```bash
codeq serve --port 8080 --redis-addr localhost:6666
```

### Create task via CLI

```bash
codeq task create \
  --command PROCESS_ORDER \
  --payload '{"orderId":"123"}' \
  --priority 5 \
  --token $PRODUCER_TOKEN
```

### Claim task via CLI

```bash
codeq task claim \
  --commands PROCESS_ORDER \
  --lease 120 \
  --wait 10 \
  --token $WORKER_TOKEN
```

## Go SDK Examples

The Go SDK lives inside the main module under
[`pkg/producerclient`](../pkg/producerclient) (task creation) and
[`pkg/workerclient`](../pkg/workerclient) (claim + result). Both use
gRPC streams under the hood.

**Install:**

```bash
go get github.com/osvaldoandrade/codeq
```

**Create a task (producer):**

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

sess, err := cli.Connect(ctx)
if err != nil {
    return err
}
defer sess.Close()

taskID, err := sess.Produce(ctx, producerclient.CreateRequest{
    Command:  "PROCESS_ORDER",
    Payload:  []byte(`{"orderId":"123","amount":99.99}`),
    Priority: 5,
})
```

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

**See also**:
- [Go Integration Guide](integrations/go-integration.md) — full surface
  walk-through.
- [Streaming API guide](34-streaming-api-guide.md) — high-throughput
  producer/worker stream patterns.

For non-Go callers, use the HTTP API documented in
[04-http-api.md](04-http-api.md).

## OpenTelemetry Distributed Tracing

### Basic Configuration

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

### Trace Context Propagation

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

### Example: End-to-End Tracing with Jaeger

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

### Tracing with Custom Services

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

### Sampling Configuration

Control what percentage of traces are sampled:

````yaml
tracingSampleRatio: 0.1  # Sample 10% of requests
````

Sampling is parent-based by default, so if an incoming request has a sampled trace context, codeQ will honor it.

## Additional Resources

- **Go SDK overview**: [sdks/README.md](../sdks/README.md)
- **HTTP API Reference**: [04-http-api.md](04-http-api.md)
- **CLI Reference**: [15-cli-reference.md](15-cli-reference.md)
- **Tracing Configuration**: [14-configuration.md](14-configuration.md#tracing-opentelemetry)
- **Operations Guide**: [10-operations.md](10-operations.md#tracing-opentelemetry)
- **Examples directory**: [examples/](../examples/)
- **Go Integration Guide**: [integrations/go-integration.md](integrations/go-integration.md)
