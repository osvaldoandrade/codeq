# Spring Boot + CodeQ Integration Example

Complete Spring Boot example demonstrating CodeQ integration as both producer and worker.

## 📦 What's Included

This example demonstrates:
- **Producer Pattern**: REST API endpoints for creating tasks
- **Worker Pattern**: Background service with scheduled task processing
- **Spring Boot 3.x**: Modern Spring Boot with Java 17+
- **Configuration Management**: Externalized configuration with `application.properties`
- **Heartbeat Management**: Automatic lease extension for long-running tasks
- **Error Handling**: NACK on failure with retry logic
- **Health Checks**: Spring Actuator integration for monitoring

## 🏗️ Architecture

````
┌─────────────────────────────────────────────────┐
│       Spring Boot Application                   │
│                                                 │
│  ┌──────────────┐        ┌─────────────────┐   │
│  │   Producer   │        │     Worker      │   │
│  │ (Controller) │        │  (@Scheduled)   │   │
│  └──────────────┘        └─────────────────┘   │
│         │                        │              │
│         └────────┬───────────────┘              │
│                  │                              │
│         ┌────────▼────────┐                     │
│         │  CodeQ Client   │                     │
│         │  (CodeQConfig)  │                     │
│         └────────┬────────┘                     │
└──────────────────┼──────────────────────────────┘
                   │ HTTP
          ┌────────▼────────┐
          │  CodeQ Server   │
          │   (port 8080)   │
          └────────┬────────┘
                   │
          ┌────────▼────────┐
          │    KVRocks      │
          │   (Redis API)   │
          └─────────────────┘
````

## 🚀 Quick Start

### Prerequisites

- Java 17 or higher
- Maven 3.8+
- CodeQ server running (see [Local Development Guide](../../docs/22-local-development.md))
- Producer and worker authentication tokens

### 1. Configure Environment

Set environment variables or edit `src/main/resources/application.properties`:

````bash
export CODEQ_PRODUCER_TOKEN="your-producer-token"
export CODEQ_WORKER_TOKEN="your-worker-token"
````

**Getting Tokens**: See [Authentication Guide](../../docs/09-security.md) for obtaining JWT tokens.

### 2. Build and Run

````bash
cd examples/java/springboot

# Development mode with Maven
mvn spring-boot:run

# Or build and run JAR
mvn clean package
java -jar target/codeq-springboot-example-1.0.0.jar
````

The application starts on `http://localhost:8080` (configurable via `server.port`).

## 📚 Project Structure

````
src/main/java/io/codeq/examples/springboot/
├── CodeQSpringBootApplication.java    # Main application class
├── config/
│   └── CodeQConfig.java               # CodeQ client configuration
├── controller/
│   └── TaskController.java            # REST endpoints (Producer)
├── service/
│   └── TaskProducerService.java       # Task creation logic
└── worker/
    └── CodeQWorker.java               # Background worker (Consumer)

src/main/resources/
└── application.properties             # Application configuration
````

## 🔧 Usage Examples

### Creating Tasks (Producer)

#### 1. Create Basic Task

````bash
curl -X POST http://localhost:8080/api/tasks/master \
  -H "Content-Type: application/json" \
  -d '{
    "jobId": "job-123",
    "priority": 5
  }'
````

**Response:**
````json
{
  "id": "01JGXY...",
  "command": "GENERATE_MASTER",
  "payload": { "jobId": "job-123" },
  "priority": 5,
  "status": "ENQUEUED",
  "createdAt": "2026-02-16T00:00:00Z"
}
````

#### 2. Create Task with Webhook

````bash
curl -X POST http://localhost:8080/api/tasks/with-webhook \
  -H "Content-Type: application/json" \
  -d '{
    "command": "GENERATE_MASTER",
    "payload": { "jobId": "job-456" },
    "webhook": "https://your-app.com/webhooks/task-complete"
  }'
````

When the task completes, CodeQ sends a POST request to your webhook with the result.

#### 3. Create Delayed Task

````bash
curl -X POST http://localhost:8080/api/tasks/delayed \
  -H "Content-Type: application/json" \
  -d '{
    "command": "GENERATE_MASTER",
    "payload": { "jobId": "job-789" },
    "delaySeconds": 60
  }'
````

Task is enqueued but not claimable until 60 seconds have passed.

### Processing Tasks (Worker)

The worker component (`CodeQWorker`) automatically:
1. **Polls** for available tasks every 5 seconds
2. **Claims** tasks with 120-second lease
3. **Sends heartbeats** every 30 seconds during processing
4. **Processes** task using command-specific logic
5. **Submits results** or **NACKs** on failure

#### Worker Output

````
2026-02-16 10:30:45 - Claimed task: 01JGXY... (command: GENERATE_MASTER)
2026-02-16 10:30:45 - Processing job: job-123
2026-02-16 10:31:15 - Heartbeat sent for task: 01JGXY...
2026-02-16 10:31:30 - Task completed: 01JGXY...
````

## 🔑 Key Components

### CodeQ Configuration (`config/CodeQConfig.java`)

Spring `@Configuration` that provides `CodeQClient` as a bean:

````java
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
        return new CodeQClient.Builder()
            .baseUrl(baseUrl)
            .producerToken(producerToken)
            .workerToken(workerToken)
            .timeout(30000)
            .retries(3)
            .build();
    }
}
````

### Task Controller (`controller/TaskController.java`)

REST endpoints for task creation:
- `POST /api/tasks/master` - Create task with priority
- `POST /api/tasks/with-webhook` - Create task with result webhook
- `POST /api/tasks/delayed` - Create delayed task

### Worker Component (`worker/CodeQWorker.java`)

Background worker with two scheduled methods:

````java
@Component
@RequiredArgsConstructor
@Slf4j
public class CodeQWorker {
    
    private final CodeQClient codeQClient;
    private Task currentTask;
    
    @Scheduled(fixedDelay = 5000)
    public void pollAndProcess() {
        Task task = codeQClient.claimTask(
            List.of("GENERATE_MASTER", "GENERATE_CREATIVE"),
            120, // lease seconds
            10   // wait seconds (long-polling)
        );
        
        if (task != null) {
            processTask(task);
        }
    }
    
    @Scheduled(fixedDelay = 30000)
    public void sendHeartbeat() {
        if (currentTask != null) {
            codeQClient.heartbeat(currentTask.getId(), 60);
        }
    }
}
````

## 🎯 Best Practices

### 1. Dependency Injection

Use constructor injection with Lombok:

````java
@Component
@RequiredArgsConstructor
public class MyService {
    private final CodeQClient codeQClient;
}
````

### 2. Configuration Externalization

Use `application.properties` with environment variable overrides:

````properties
codeq.base-url=${CODEQ_BASE_URL:http://localhost:8080}
codeq.producer-token=${CODEQ_PRODUCER_TOKEN}
codeq.worker-token=${CODEQ_WORKER_TOKEN}
````

### 3. Long-Polling for Efficiency

````java
Task task = codeQClient.claimTask(
    commands,
    120,  // lease seconds
    10    // wait seconds for long-polling
);
````

Reduces HTTP requests when queues are empty.

### 4. Heartbeat Timing

Send heartbeats at 1/3 to 1/2 of the lease duration:
- Lease: 120 seconds → Heartbeat every 30-60 seconds
- Lease: 60 seconds → Heartbeat every 20-30 seconds

### 5. NACK on Transient Failures

````java
try {
    processTask(task);
    codeQClient.submitResult(task.getId(), Map.of("success", true));
} catch (Exception e) {
    if (isTransientError(e)) {
        // Retry with backoff
        codeQClient.nack(task.getId(), Map.of(
            "error", e.getMessage(),
            "willRetry", true
        ));
    } else {
        // Permanent failure
        codeQClient.submitResult(task.getId(), Map.of(
            "success", false,
            "error", e.getMessage()
        ));
    }
}
````

### 6. Graceful Shutdown

Implement `DisposableBean`:

````java
@Component
public class CodeQWorker implements DisposableBean {
    
    @Override
    public void destroy() throws Exception {
        if (currentTask != null) {
            codeQClient.abandon(currentTask.getId());
            log.info("Abandoned task on shutdown: {}", currentTask.getId());
        }
    }
}
````

## 🧪 Testing

````bash
# Run tests
mvn test

# Run with test profile
mvn spring-boot:run -Dspring-boot.run.profiles=test
````

## 📊 Production Considerations

### Scaling Workers

Run multiple instances with different ports:

````bash
# Instance 1
mvn spring-boot:run -Dspring-boot.run.arguments=--server.port=8081

# Instance 2
mvn spring-boot:run -Dspring-boot.run.arguments=--server.port=8082
````

Each instance claims and processes tasks independently. CodeQ ensures no duplicate processing via lease mechanism.

### Health Checks

Spring Actuator provides built-in health checks:

````bash
# Health endpoint
curl http://localhost:8080/actuator/health

# Metrics
curl http://localhost:8080/actuator/metrics
````

Add custom health indicator:

````java
@Component
public class CodeQHealthIndicator implements HealthIndicator {
    
    private final CodeQClient codeQClient;
    
    @Override
    public Health health() {
        try {
            // Ping CodeQ server
            codeQClient.getQueueStats("health-check");
            return Health.up().build();
        } catch (Exception e) {
            return Health.down().withException(e).build();
        }
    }
}
````

### Logging Configuration

Configure logging levels in `application.properties`:

````properties
# Root level
logging.level.root=INFO

# CodeQ SDK debug logs
logging.level.io.codeq=DEBUG

# Your application
logging.level.io.codeq.examples=DEBUG
````

### Production Profile

Create `application-prod.properties`:

````properties
server.port=8080
codeq.base-url=https://codeq.prod.example.com
logging.level.root=WARN
management.endpoints.web.exposure.include=health,metrics
````

Run with production profile:

````bash
java -jar target/codeq-springboot-example-1.0.0.jar --spring.profiles.active=prod
````

## 📦 Maven Dependency

To use CodeQ SDK in your own project, add:

````xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-sdk-java</artifactId>
    <version>1.0.0</version>
</dependency>
````

## 🔗 Related Documentation

- [CodeQ Getting Started](../../docs/00-getting-started.md)
- [Java Integration Guide](../../docs/integrations/java-integration.md)
- [HTTP API Reference](../../docs/04-http-api.md)
- [SDK Documentation](../../sdks/README.md)
- [Local Development](../../docs/22-local-development.md)

## 🐛 Troubleshooting

### Issue: "Connection refused" when creating tasks

**Solution**: Ensure CodeQ server is running on configured URL:
````bash
cd ../..
docker compose up -d
````

### Issue: "Unauthorized" error

**Solution**: Verify your tokens are valid JWT tokens with correct scopes. See [Authentication Guide](../../docs/09-security.md).

### Issue: Worker not claiming tasks

**Solution**: Check:
1. Worker token has `codeq:claim` and `codeq:result` scopes
2. Task command is in the worker's `commands` list
3. Task is not already claimed by another worker
4. Ensure `@EnableScheduling` is present on main application class

### Issue: Scheduled methods not running

**Solution**: Add `@EnableScheduling` to your `@SpringBootApplication` class:
````java
@SpringBootApplication
@EnableScheduling
public class CodeQSpringBootApplication {
    // ...
}
````

### Issue: Tasks timing out

**Solution**: Increase lease duration or send heartbeats more frequently:
````java
Task task = codeQClient.claimTask(
    commands,
    300,  // 5 minutes lease
    10
);
````

## 💡 Additional Examples

### Custom Task Processing

````java
private void processTask(Task task) {
    switch (task.getCommand()) {
        case "GENERATE_MASTER":
            processMasterGeneration(task);
            break;
        case "GENERATE_CREATIVE":
            processCreativeGeneration(task);
            break;
        default:
            log.warn("Unknown command: {}", task.getCommand());
            codeQClient.abandon(task.getId());
    }
}
````

### Async Processing with CompletableFuture

````java
@Async
public CompletableFuture<Void> processTaskAsync(Task task) {
    return CompletableFuture.runAsync(() -> {
        try {
            // Long-running processing
            processTask(task);
        } catch (Exception e) {
            log.error("Async task processing failed", e);
        }
    });
}
````

## 📝 License

This example is part of the CodeQ project and is available under the same license.
