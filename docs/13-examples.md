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

**Integration guide**: [Java Integration Guide](integrations/21-java-integration.md)

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

**Integration guide**: [Node.js Integration Guide](integrations/22-nodejs-integration.md)

## Additional Resources

- **SDK Documentation**: [sdks/README.md](../sdks/README.md)
- **HTTP API Reference**: [04-http-api.md](04-http-api.md)
- **CLI Reference**: [15-cli-reference.md](15-cli-reference.md)
- **All Examples**: [examples/](../examples/)
- **Integration Guides**:
  - [Java Integration](integrations/21-java-integration.md)
  - [Node.js Integration](integrations/22-nodejs-integration.md)

