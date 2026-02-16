# Usage Examples

This document provides practical examples of using CodeQ via HTTP API, CLI, and official SDKs.

## Table of Contents

- [HTTP API Examples](#http-api-examples)
- [CLI Examples](#cli-examples)
- [SDK Examples](#sdk-examples)
- [Framework Examples](#framework-examples)

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

## SDK Examples

### Java SDK

**Create a task**:

```java
CodeQClient client = CodeQClient.builder()
    .baseUrl("https://codeq.example.com")
    .producerToken(System.getenv("CODEQ_PRODUCER_TOKEN"))
    .workerToken(System.getenv("CODEQ_WORKER_TOKEN"))
    .build();

// Create task
Map<String, Object> payload = Map.of("orderId", "123", "amount", 99.99);
Task task = client.createTask("PROCESS_ORDER", payload, 5);
System.out.println("Created task: " + task.getId());
```

**Claim and process task**:

```java
// Claim task
Task task = client.claimTask(
    List.of("PROCESS_ORDER"), 
    120,  // lease seconds
    10    // wait seconds
);

if (task != null) {
    try {
        // Process task
        processOrder(task.getPayload());
        
        // Submit result
        Map<String, Object> result = Map.of("status", "success");
        client.submitResult(task.getId(), result);
    } catch (Exception e) {
        // NACK on failure
        client.nackTask(task.getId(), "Processing failed: " + e.getMessage());
    }
}
```

**Complete example**: See [examples/java/springboot](../examples/java/springboot/)

---

### Node.js/TypeScript SDK

**Create a task**:

```typescript
import { CodeQClient } from '@codeq/sdk';

const client = new CodeQClient({
  baseUrl: 'https://codeq.example.com',
  producerToken: process.env.CODEQ_PRODUCER_TOKEN,
  workerToken: process.env.CODEQ_WORKER_TOKEN,
});

// Create task
const task = await client.createTask({
  command: 'PROCESS_ORDER',
  payload: { orderId: '123', amount: 99.99 },
  priority: 5,
});
console.log('Created task:', task.id);
```

**Claim and process task**:

```typescript
// Claim task
const task = await client.claimTask({
  commands: ['PROCESS_ORDER'],
  leaseSeconds: 120,
  waitSeconds: 10,
});

if (task) {
  try {
    // Process task
    await processOrder(task.payload);
    
    // Submit result
    await client.submitResult(task.id, {
      status: 'COMPLETED',
      result: { success: true },
    });
  } catch (error) {
    // NACK on failure
    await client.nackTask(task.id, `Processing failed: ${error.message}`);
  }
}
```

**Complete example**: See [examples/nodejs/nestjs](../examples/nodejs/nestjs/)

## Framework Examples

### Spring Boot Integration

**Configuration**:

```java
@Configuration
public class CodeQConfig {
    @Bean
    public CodeQClient codeQClient(
        @Value("${codeq.base-url}") String baseUrl,
        @Value("${codeq.producer-token}") String producerToken,
        @Value("${codeq.worker-token}") String workerToken
    ) {
        return CodeQClient.builder()
            .baseUrl(baseUrl)
            .producerToken(producerToken)
            .workerToken(workerToken)
            .build();
    }
}
```

**Producer Service**:

```java
@Service
public class OrderService {
    @Autowired
    private CodeQClient codeQClient;
    
    public String createOrderTask(Order order) {
        Task task = codeQClient.createTask(
            "PROCESS_ORDER",
            Map.of("orderId", order.getId()),
            5
        );
        return task.getId();
    }
}
```

**Worker Service**:

```java
@Service
public class OrderWorker {
    @Autowired
    private CodeQClient codeQClient;
    
    @Scheduled(fixedDelay = 5000)
    public void pollTasks() {
        Task task = codeQClient.claimTask(
            List.of("PROCESS_ORDER"),
            120,
            10
        );
        
        if (task != null) {
            processTask(task);
        }
    }
    
    private void processTask(Task task) {
        try {
            // Process order
            Map<String, Object> result = Map.of("status", "success");
            codeQClient.submitResult(task.getId(), result);
        } catch (Exception e) {
            codeQClient.nackTask(task.getId(), e.getMessage());
        }
    }
}
```

**Full example**: [examples/java/springboot](../examples/java/springboot/)

**Integration guide**: [Java Integration Guide](integrations/java-integration.md)

---

### NestJS Integration

**Module Configuration**:

```typescript
@Module({
  providers: [
    {
      provide: 'CODEQ_CLIENT',
      useFactory: () => {
        return new CodeQClient({
          baseUrl: process.env.CODEQ_BASE_URL,
          producerToken: process.env.CODEQ_PRODUCER_TOKEN,
          workerToken: process.env.CODEQ_WORKER_TOKEN,
        });
      },
    },
  ],
  exports: ['CODEQ_CLIENT'],
})
export class CodeQModule {}
```

**Producer Controller**:

```typescript
@Controller('orders')
export class OrdersController {
  constructor(@Inject('CODEQ_CLIENT') private codeQClient: CodeQClient) {}

  @Post()
  async createOrder(@Body() order: CreateOrderDto) {
    const task = await this.codeQClient.createTask({
      command: 'PROCESS_ORDER',
      payload: { orderId: order.id },
      priority: 5,
    });
    return { taskId: task.id };
  }
}
```

**Worker Service**:

```typescript
@Injectable()
export class OrderWorker {
  constructor(@Inject('CODEQ_CLIENT') private codeQClient: CodeQClient) {}

  @Cron(CronExpression.EVERY_5_SECONDS)
  async pollTasks() {
    const task = await this.codeQClient.claimTask({
      commands: ['PROCESS_ORDER'],
      leaseSeconds: 120,
      waitSeconds: 10,
    });

    if (task) {
      await this.processTask(task);
    }
  }

  private async processTask(task: Task) {
    try {
      // Process order
      await this.codeQClient.submitResult(task.id, {
        status: 'COMPLETED',
        result: { success: true },
      });
    } catch (error) {
      await this.codeQClient.nackTask(task.id, error.message);
    }
  }
}
```

**Full example**: [examples/nodejs/nestjs](../examples/nodejs/nestjs/)

**Integration guide**: [Node.js Integration Guide](integrations/nodejs-integration.md)

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
docker compose --profile obs up -d

# Enable tracing in .env
cat >> .env << 'EOF'
TRACING_ENABLED=true
TRACING_SERVICE_NAME=codeq
TRACING_OTLP_ENDPOINT=jaeger:4317
TRACING_OTLP_INSECURE=true
TRACING_SAMPLE_RATIO=1.0
EOF

# Restart codeQ
docker compose restart codeq

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

- **SDK Documentation**: [sdks/README.md](../sdks/README.md)
- **HTTP API Reference**: [04-http-api.md](04-http-api.md)
- **CLI Reference**: [15-cli-reference.md](15-cli-reference.md)
- **Tracing Configuration**: [14-configuration.md](14-configuration.md#tracing-opentelemetry)
- **Operations Guide**: [10-operations.md](10-operations.md#tracing-opentelemetry)
- **All Examples**: [examples/](../examples/)
- **Integration Guides**:
  - [Java Integration](integrations/java-integration.md)
  - [Node.js Integration](integrations/nodejs-integration.md)

