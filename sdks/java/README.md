# CodeQ Java SDK

Official Java client SDK for [codeQ](https://github.com/osvaldoandrade/codeq) — a distributed task queue.

## Features

- **Zero configuration** — sensible defaults with builder pattern for customization
- **Full API coverage** — producer, worker, admin, and subscription operations
- **Type-safe** — strongly typed request/response models with Java records
- **Automatic retry** — configurable exponential back-off for transient failures (5xx)
- **Connection pooling** — powered by OkHttp for efficient resource usage
- **Thread-safe** — safe for concurrent use from multiple threads
- **Comprehensive logging** — SLF4J integration for debugging and monitoring

## Requirements

- Java 17 or later
- A running codeQ server
- Maven or Gradle for dependency management

## Installation

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

## Quick Start

### Producer — create tasks

```java
package com.example;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.Task;
import java.util.Map;

public class ProducerExample {
    public static void main(String[] args) throws Exception {
        CodeQClient client = CodeQClient.builder()
            .baseUrl("http://localhost:8080")
            .producerToken("your-producer-jwt")
            .build();

        Task task = client.createTask(
            "PROCESS_IMAGE",
            Map.of("url", "https://example.com/image.png"),
            5  // priority
        );

        System.out.printf("Created task %s (status: %s)%n", task.getId(), task.getStatus());
    }
}
```

### Worker — claim and complete tasks

```java
package com.example;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.Task;
import java.util.List;
import java.util.Map;

public class WorkerExample {
    public static void main(String[] args) throws Exception {
        CodeQClient client = CodeQClient.builder()
            .baseUrl("http://localhost:8080")
            .workerToken("your-worker-jwt")
            .build();

        Task task = client.claimTask(
            List.of("PROCESS_IMAGE"),  // commands to claim
            120,                         // lease duration in seconds
            30                           // wait time in seconds
        );

        if (task == null) {
            System.out.println("No tasks available");
            return;
        }

        System.out.printf("Claimed task %s%n", task.getId());

        // Process the task …
        Map<String, Object> result = Map.of("output", "processed");

        client.submitResult(task.getId(), result);
        System.out.printf("Result submitted for %s%n", task.getId());
    }
}
```

## API Reference

### Client Configuration

```java
CodeQClient client = CodeQClient.builder()
    .baseUrl(String baseUrl)
    .producerToken(String token)           // optional
    .workerToken(String token)              // optional
    .adminToken(String token)               // optional
    .httpClient(OkHttpClient client)        // optional
    .maxRetries(int retries)                // optional, default: 3
    .retryBaseDelayMs(long delayMs)         // optional, default: 500ms
    .requestTimeoutSeconds(long seconds)    // optional, default: 30s
    .build();
```

### Producer Operations

| Method | Description |
|--------|-------------|
| `createTask(command, payload, priority)` | Create a single task |
| `createTask(command, payload, priority, webhook, maxAttempts, delaySeconds, idempotencyKey)` | Create task with full options |
| `createTaskBatch(List<CreateTaskRequest>)` | Create up to 100 tasks in one request |

### Worker Operations

| Method | Description |
|--------|-------------|
| `claimTask(commands, leaseSeconds, waitSeconds)` | Claim a task (returns `null` when none available) |
| `claimTaskBatch(commands, count, leaseSeconds)` | Claim up to 10 tasks |
| `submitResult(taskId, result)` | Submit task result |
| `submitResult(taskId, status, result)` | Submit result with explicit status |
| `submitResultBatch(List<SubmitResultRequest>)` | Submit up to 100 results |
| `heartbeat(taskId, extendSeconds)` | Extend task lease |
| `abandon(taskId)` | Return task to queue |
| `nack(taskId, delaySeconds, reason)` | Negative ack with retry delay |

### Subscription Operations

| Method | Description |
|--------|-------------|
| `createSubscription(callbackUrl, eventTypes, ttlSeconds)` | Register a webhook subscription |
| `createSubscription(callbackUrl, eventTypes, ttlSeconds, deliveryMode, groupId)` | Create subscription with delivery mode |
| `renewSubscription(subscriptionId, ttlSeconds)` | Renew subscription lifetime |

### Query Operations

| Method | Description |
|--------|-------------|
| `getTask(taskId)` | Get task details |
| `getResult(taskId)` | Get task result |
| `waitForResult(taskId, timeoutSeconds)` | Poll until result is available |

### Admin Operations

| Method | Description |
|--------|-------------|
| `listQueues()` | List all queue statistics |
| `getQueueStats(command)` | Get stats for a single queue |
| `cleanupExpired(limit)` | Remove expired tasks |

## Error Handling

The SDK defines error types for different failure scenarios:

```java
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.CodeQException.*;

try {
    Task task = client.createTask("PROCESS", Map.of("id", "123"), 5);
} catch (CodeQException e) {
    // Handle generic SDK error
    System.err.println("SDK error: " + e.getMessage());
} catch (Exception e) {
    System.err.println("Unexpected error: " + e.getMessage());
}
```

Common exceptions:

- `CodeQException` — Base exception wrapping root cause
- `CodeQException.ApiError` — HTTP 4xx/5xx response from the API
- `CodeQException.AuthError` — HTTP 401 or 403
- `CodeQException.TimeoutError` — Request timeout or polling timeout

Use try-catch blocks with specific exception types:

```java
try {
    Task task = client.createTask("PROCESS", payload, priority);
} catch (CodeQException.AuthError e) {
    System.err.println("Authentication failed: " + e.getMessage());
} catch (CodeQException.ApiError e) {
    System.err.println("API error: " + e.getStatusCode());
} catch (CodeQException e) {
    System.err.println("Other SDK error: " + e.getMessage());
}
```

## Configuration via Environment Variables

A common pattern is to configure the client from environment variables:

```java
CodeQClient client = CodeQClient.builder()
    .baseUrl(System.getenv("CODEQ_BASE_URL"))
    .producerToken(System.getenv("CODEQ_PRODUCER_TOKEN"))
    .workerToken(System.getenv("CODEQ_WORKER_TOKEN"))
    .build();
```

## Retry Behavior

By default, the client retries up to 3 times with exponential back-off
(500 ms, 1 s, 2 s) on 5xx server errors and network failures. Client errors
(4xx) are never retried.

```java
// Disable retries
CodeQClient client = CodeQClient.builder()
    .baseUrl(url)
    .maxRetries(0)
    .build();

// Custom retry configuration
CodeQClient client = CodeQClient.builder()
    .baseUrl(url)
    .maxRetries(5)
    .retryBaseDelayMs(1000)
    .build();
```

## Testing

```bash
cd sdks/java/core
mvn test
```

## Integration Guides

For framework-specific integration patterns, see the integration guides:

- **Spring Boot** — `docs/integrations/java-integration.md#spring-boot-integration`
- **Quarkus** — `docs/integrations/java-integration.md#quarkus-integration`
- **Micronaut** — `docs/integrations/java-integration.md#micronaut-integration`

## Common Patterns

### Long-polling for tasks

```java
while (true) {
    Task task = client.claimTask(
        List.of("PROCESS_IMAGE", "GENERATE_REPORT"),
        120,    // lease duration
        30      // wait timeout (long-poll)
    );
    
    if (task == null) {
        continue;  // No tasks available
    }
    
    try {
        processTask(task);
        client.submitResult(task.getId(), Map.of("status", "success"));
    } catch (Exception e) {
        client.nack(task.getId(), 60, e.getMessage());
    }
}
```

### Batch operations

```java
// Create multiple tasks efficiently
List<Map<String, Object>> tasks = List.of(
    Map.of("command", "RENDER", "payload", Map.of("frame", 1)),
    Map.of("command", "RENDER", "payload", Map.of("frame", 2)),
    Map.of("command", "RENDER", "payload", Map.of("frame", 3))
);

List<Task> created = client.createTaskBatch(tasks);
for (Task task : created) {
    System.out.printf("Created task %s%n", task.getId());
}
```

### Heartbeat for long-running tasks

```java
Task task = client.claimTask(List.of("LONG_RUNNING_JOB"), 120, 30);

// Do work in chunks
for (int i = 0; i < chunks.size(); i++) {
    processChunk(chunks.get(i));
    
    // Extend lease before it expires
    if (i % 10 == 0) {
        client.heartbeat(task.getId(), 120);
    }
}

client.submitResult(task.getId(), Map.of("status", "completed"));
```

## License

This SDK is part of the [codeQ](https://github.com/osvaldoandrade/codeq) project and is released under the same license.
