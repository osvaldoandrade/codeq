# Java Integration Guide

Complete guide for integrating CodeQ with Java microservices using Spring Boot, Quarkus, and Micronaut.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Spring Boot Integration](#spring-boot-integration)
- [Quarkus Integration](#quarkus-integration)
- [Micronaut Integration](#micronaut-integration)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Java SDK provides a simple, type-safe API for:
- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations

### Architecture

```
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
```

## SDK Installation

### Maven

Add to your `pom.xml`:

```xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-sdk-java</artifactId>
    <version>1.0.0</version>
</dependency>
```

### Gradle

Add to your `build.gradle`:

```gradle
implementation 'io.codeq:codeq-sdk-java:1.0.0'
```

## Spring Boot Integration

### 1. Configuration

Create a configuration class:

```java
@Configuration
public class CodeQConfig {

    @Value("${codeq.base-url}")
    private String baseUrl;

    @Value("${codeq.producer-token}")
    private String producerToken;

    @Value("${codeq.worker-token}")
    private String workerToken;

    @Bean
    public CodeQClient codeQClient() {
        return CodeQClient.builder()
                .baseUrl(baseUrl)
                .producerToken(producerToken)
                .workerToken(workerToken)
                .build();
    }
}
```

Add to `application.properties`:

```properties
codeq.base-url=https://codeq.example.com
codeq.producer-token=${CODEQ_PRODUCER_TOKEN}
codeq.worker-token=${CODEQ_WORKER_TOKEN}
```

### 2. Producer Service

Create tasks from your business logic:

```java
@Service
@RequiredArgsConstructor
public class OrderService {

    private final CodeQClient codeQClient;

    public Order createOrder(OrderRequest request) {
        // Save order to database
        Order order = orderRepository.save(request);

        // Enqueue async processing task
        try {
            codeQClient.createTask(
                "PROCESS_ORDER",
                Map.of("orderId", order.getId()),
                5 // priority
            );
        } catch (CodeQException e) {
            log.error("Failed to enqueue order processing", e);
            // Handle error (retry, alert, etc.)
        }

        return order;
    }
}
```

### 3. Worker Service

Process tasks in background:

```java
@Component
@RequiredArgsConstructor
@Slf4j
public class OrderWorker {

    private final CodeQClient codeQClient;
    private final OrderProcessor orderProcessor;
    private Task currentTask;

    @Scheduled(fixedDelay = 5000)
    public void pollAndProcess() {
        if (currentTask != null) return;

        try {
            Task task = codeQClient.claimTask(
                List.of("PROCESS_ORDER"),
                120, // lease seconds
                10   // wait seconds
            );

            if (task == null) return;

            currentTask = task;
            processTask(task);

        } catch (CodeQException e) {
            log.error("Error claiming task", e);
        }
    }

    @Scheduled(fixedDelay = 30000)
    public void sendHeartbeat() {
        if (currentTask == null) return;

        try {
            codeQClient.heartbeat(currentTask.getId(), 60);
        } catch (CodeQException e) {
            log.error("Heartbeat failed", e);
        }
    }

    private void processTask(Task task) {
        try {
            String orderId = (String) task.getPayload().get("orderId");
            Map<String, Object> result = orderProcessor.process(orderId);

            codeQClient.submitResult(task.getId(), result);
            log.info("Task completed: {}", task.getId());

        } catch (Exception e) {
            handleError(task, e);
        } finally {
            currentTask = null;
        }
    }

    private void handleError(Task task, Exception e) {
        try {
            if (isRetryable(e)) {
                codeQClient.nack(task.getId(), 30, e.getMessage());
            } else {
                codeQClient.submitResult(
                    task.getId(),
                    "FAILED",
                    Map.of("error", e.getMessage()),
                    e.getMessage()
                );
            }
        } catch (CodeQException ex) {
            log.error("Error handling task failure", ex);
        }
    }

    private boolean isRetryable(Exception e) {
        return !(e instanceof IllegalArgumentException);
    }
}
```

## Quarkus Integration

### 1. Configuration

Add to `application.properties`:

```properties
codeq.base-url=https://codeq.example.com
codeq.producer-token=${CODEQ_PRODUCER_TOKEN}
codeq.worker-token=${CODEQ_WORKER_TOKEN}
```

Create producer:

```java
@ApplicationScoped
public class CodeQProducer {

    @ConfigProperty(name = "codeq.base-url")
    String baseUrl;

    @ConfigProperty(name = "codeq.producer-token")
    String producerToken;

    @ConfigProperty(name = "codeq.worker-token")
    String workerToken;

    private CodeQClient client;

    @PostConstruct
    void init() {
        client = CodeQClient.builder()
                .baseUrl(baseUrl)
                .producerToken(producerToken)
                .workerToken(workerToken)
                .build();
    }

    @Produces
    @ApplicationScoped
    public CodeQClient codeQClient() {
        return client;
    }
}
```

### 2. Reactive Worker

Use Quarkus scheduler:

```java
@ApplicationScoped
public class QuarkusWorker {

    @Inject
    CodeQClient codeQClient;

    @Inject
    Logger log;

    private Task currentTask;

    @Scheduled(every = "5s")
    void pollTasks() {
        if (currentTask != null) return;

        try {
            Task task = codeQClient.claimTask(
                List.of("PROCESS_ORDER"),
                120,
                10
            );

            if (task != null) {
                currentTask = task;
                processTask(task);
            }
        } catch (CodeQException e) {
            log.error("Error claiming task", e);
        }
    }

    private void processTask(Task task) {
        // Process task...
        currentTask = null;
    }
}
```

## Micronaut Integration

### 1. Configuration

Create `application.yml`:

```yaml
codeq:
  base-url: https://codeq.example.com
  producer-token: ${CODEQ_PRODUCER_TOKEN}
  worker-token: ${CODEQ_WORKER_TOKEN}
```

Create factory:

```java
@Factory
public class CodeQFactory {

    @Bean
    @Singleton
    public CodeQClient codeQClient(
        @Property(name = "codeq.base-url") String baseUrl,
        @Property(name = "codeq.producer-token") String producerToken,
        @Property(name = "codeq.worker-token") String workerToken
    ) {
        return CodeQClient.builder()
                .baseUrl(baseUrl)
                .producerToken(producerToken)
                .workerToken(workerToken)
                .build();
    }
}
```

### 2. Scheduled Worker

```java
@Singleton
public class MicronautWorker {

    private final CodeQClient codeQClient;
    private Task currentTask;

    public MicronautWorker(CodeQClient codeQClient) {
        this.codeQClient = codeQClient;
    }

    @Scheduled(fixedDelay = "5s")
    void pollTasks() {
        if (currentTask != null) return;

        try {
            Task task = codeQClient.claimTask(
                List.of("PROCESS_ORDER"),
                120,
                10
            );

            if (task != null) {
                currentTask = task;
                processTask(task);
            }
        } catch (CodeQException e) {
            // Handle error
        }
    }

    private void processTask(Task task) {
        // Process task...
        currentTask = null;
    }
}
```

## Best Practices

### 1. Connection Pooling

Configure OkHttp connection pool:

```java
OkHttpClient httpClient = new OkHttpClient.Builder()
    .connectionPool(new ConnectionPool(10, 5, TimeUnit.MINUTES))
    .connectTimeout(Duration.ofSeconds(10))
    .readTimeout(Duration.ofSeconds(30))
    .build();

CodeQClient client = CodeQClient.builder()
    .baseUrl(baseUrl)
    .producerToken(producerToken)
    .workerToken(workerToken)
    .httpClient(httpClient)
    .build();
```

### 2. Error Handling

Always handle exceptions:

```java
try {
    Task task = codeQClient.createTask(command, payload, priority);
} catch (CodeQException e) {
    // Log error
    log.error("Failed to create task", e);
    
    // Retry with exponential backoff
    retryService.scheduleRetry(() -> createTask(...));
    
    // Or store in local queue for later retry
    localQueue.add(new PendingTask(command, payload));
}
```

### 3. Graceful Shutdown

Abandon tasks on shutdown:

```java
@PreDestroy
public void shutdown() {
    if (currentTask != null) {
        try {
            codeQClient.abandon(currentTask.getId());
            log.info("Abandoned task on shutdown: {}", currentTask.getId());
        } catch (CodeQException e) {
            log.error("Failed to abandon task", e);
        }
    }
}
```

### 4. Monitoring

Add metrics:

```java
@Timed(value = "codeq.task.processing", description = "Task processing time")
private void processTask(Task task) {
    // Process task...
}

@Counted(value = "codeq.task.created", description = "Tasks created")
public Task createTask(...) {
    // Create task...
}
```

### 5. Idempotency

Use idempotency keys:

```java
String idempotencyKey = "order-" + orderId + "-processing";

Task task = codeQClient.createTask(
    "PROCESS_ORDER",
    payload,
    5,
    null,
    null,
    null,
    idempotencyKey
);
```

## Troubleshooting

### Connection Timeouts

Increase timeouts:

```java
OkHttpClient httpClient = new OkHttpClient.Builder()
    .connectTimeout(Duration.ofSeconds(30))
    .readTimeout(Duration.ofSeconds(60))
    .build();
```

### Authentication Errors

Verify tokens:

```bash
# Test producer token
curl -H "Authorization: Bearer $PRODUCER_TOKEN" \
  https://codeq.example.com/v1/codeq/tasks

# Test worker token
curl -H "Authorization: Bearer $WORKER_TOKEN" \
  https://codeq.example.com/v1/codeq/tasks/claim
```

### Task Not Claimed

Check queue stats:

```java
QueueStats stats = codeQClient.getQueueStats("PROCESS_ORDER");
log.info("Ready: {}, InProgress: {}, DLQ: {}", 
    stats.getReady(), stats.getInProgress(), stats.getDlq());
```

## See Also

- [HTTP API Reference](../04-http-api.md)
- [Configuration Guide](../14-configuration.md)
- [Performance Tuning](../17-performance-tuning.md)
- [Example: Spring Boot](../../examples/java/springboot/)
- [Example: Quarkus](../../examples/java/quarkus/)
- [Example: Micronaut](../../examples/java/micronaut/)
