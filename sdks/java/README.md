# CodeQ Java SDK

Official Java SDK for the [CodeQ](https://github.com/osvaldoandrade/codeq) reactive task scheduling system. Integrates seamlessly with Spring Boot, Quarkus, and Micronaut.

## Features

- **Full API coverage** — Task creation, claiming, results, webhooks, batch operations, and admin operations
- **Enterprise frameworks** — Spring Boot, Quarkus, Micronaut integrations via `spring-boot-starter-codeq` and equivalent starters
- **Strong typing** — Comprehensive Java models with builder patterns
- **Automatic retry** — Exponential backoff on transient failures
- **Connection pooling** — Powered by OkHttp with configurable client
- **JWT authentication** — Producer and worker token support
- **Comprehensive documentation** — Full Javadoc and integration guides

## Installation

### Maven

Add to `pom.xml`:

```xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-sdk-java</artifactId>
    <version>1.0.0</version>
</dependency>
```

### Gradle

Add to `build.gradle` or `build.gradle.kts`:

```gradle
implementation 'io.codeq:codeq-sdk-java:1.0.0'
```

## Quick Start

### Basic Setup

```java
import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.Task;
import java.util.Map;

CodeQClient client = CodeQClient.builder()
    .baseUrl("http://localhost:8080")
    .producerToken("your-producer-jwt")
    .workerToken("your-worker-jwt")
    .build();
```

### Producer — Create tasks

```java
Map<String, Object> payload = Map.of(
    "jobId", "j-123",
    "parameters", Map.of("key", "value")
);

Task task = client.createTask(
    "GENERATE_MASTER",
    payload,
    5  // priority
);

System.out.println("Created task: " + task.getId());
```

### Worker — Claim and process tasks

```java
import io.codeq.sdk.CodeQException;
import java.util.List;

try {
    Task claimed = client.claimTask(
        List.of("GENERATE_MASTER"),
        120,  // leaseSeconds
        10    // waitSeconds (long-poll)
    );
    
    if (claimed != null) {
        // Process the task
        Map<String, Object> result = processTask(claimed.getPayload());
        
        // Submit result
        client.submitResult(
            claimed.getId(),
            "COMPLETED",
            result,
            null  // error (null if successful)
        );
    }
} catch (CodeQException e) {
    System.err.println("Failed to claim task: " + e.getMessage());
}
```

### Task lifecycle

```java
// Extend lease if processing takes longer
client.heartbeat(taskId, 120);  // Extend by 120 seconds

// If processing fails, NACK to retry after delay
client.nack(taskId, 30, "Database connection timeout");

// If you want to give up, abandon the task
client.abandon(taskId);
```

## Configuration

### Builder Options

```java
CodeQClient client = CodeQClient.builder()
    .baseUrl("https://codeq.example.com")
    .producerToken("your-producer-jwt")
    .workerToken("your-worker-jwt")
    .httpClient(customOkHttpClient)  // Optional custom client
    .build();
```

### Using with Spring Boot

For Spring Boot integration, add the starter:

```xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-spring-boot-starter</artifactId>
    <version>1.0.0</version>
</dependency>
```

Configure in `application.yml` or `application.properties`:

```yaml
codeq:
  base-url: http://codeq:8080
  producer-token: ${CODEQ_PRODUCER_TOKEN}
  worker-token: ${CODEQ_WORKER_TOKEN}
```

Then inject:

```java
@Autowired
private CodeQClient codeqClient;
```

## API Reference

### Creating Tasks

```java
// Simple creation
Task task = client.createTask("COMMAND", payload, priority);

// With all options
Task task = client.createTask(
    "COMMAND",
    payload,
    priority,
    "https://myapp.com/webhook",  // webhook URL
    5,                             // maxAttempts
    10,                            // delaySeconds
    "idempotency-key-123"          // idempotency key
);
```

### Claiming Tasks

```java
Task claimed = client.claimTask(
    List.of("COMMAND1", "COMMAND2"),
    120,  // leaseSeconds (how long before lease expires)
    30    // waitSeconds (long-poll timeout)
);
```

### Submitting Results

```java
// Successful completion
client.submitResult(
    taskId,
    "COMPLETED",
    Map.of("status", "success", "data", resultData),
    null
);

// Failed completion
client.submitResult(
    taskId,
    "FAILED",
    null,
    "Processing error: Invalid input"
);
```

### Heartbeat and Control

```java
// Extend lease if needed
client.heartbeat(taskId, 120);

// NACK to retry after delay
client.nack(taskId, 30, "Transient failure");

// Abandon task permanently
client.abandon(taskId);
```

## Error Handling

All methods throw `CodeQException` on failure:

```java
try {
    Task task = client.createTask("COMMAND", payload, 5);
} catch (CodeQException e) {
    System.err.println("Error: " + e.getMessage());
    e.printStackTrace();
}
```

## Retry Behavior

The SDK automatically retries transient failures (5xx errors) with exponential backoff. For persistent failures, a `CodeQException` is thrown.

## Integration Guides

For framework-specific examples and best practices, see:

- **Spring Boot**: [docs/integrations/java-integration.md](../../docs/integrations/java-integration.md#spring-boot)
- **Quarkus**: [docs/integrations/java-integration.md](../../docs/integrations/java-integration.md#quarkus)
- **Micronaut**: [docs/integrations/java-integration.md](../../docs/integrations/java-integration.md#micronaut)

## Requirements

- Java 17 or later
- Maven 3.6+ or Gradle 6.0+
- A running CodeQ server

## Dependencies

- **OkHttp** 4.12.0 — HTTP client
- **Jackson** 2.16.1 — JSON processing
- **JJWT** 0.12.5 — JWT support
- **SLF4J** 2.0.11 — Logging API

## Examples

Working examples with Spring Boot, Quarkus, and Micronaut are available in the [examples/](../../examples/) directory.

## License

MIT License. See [LICENSE](../../LICENSE) file for details.
