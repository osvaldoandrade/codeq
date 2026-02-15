# CodeQ Quarkus Example

Quarkus example demonstrating CodeQ integration with reactive programming and native image support.

## Features

- ✅ Reactive REST API with RESTEasy Reactive
- ✅ Background worker with Quarkus Scheduler
- ✅ CDI dependency injection
- ✅ SmallRye Health checks
- ✅ Native image compilation support
- ✅ Fast startup time (~0.5s)
- ✅ Low memory footprint (~30MB)

## Prerequisites

- Java 17+
- Maven 3.8+
- GraalVM (optional, for native image)

## Running

### Development Mode

```bash
./mvnw quarkus:dev
```

Access:
- API: http://localhost:8080/api/tasks
- Health: http://localhost:8080/health

### Production Mode

```bash
./mvnw package
java -jar target/quarkus-app/quarkus-run.jar
```

### Native Image

```bash
./mvnw package -Pnative
./target/codeq-quarkus-example-1.0.0-runner
```

## API Endpoints

### Create Master Task

```bash
curl -X POST http://localhost:8080/api/tasks/master \
  -H 'Content-Type: application/json' \
  -d '{"jobId":"123","priority":5}'
```

### Create Task with Webhook

```bash
curl -X POST http://localhost:8080/api/tasks/with-webhook \
  -H 'Content-Type: application/json' \
  -d '{
    "command": "GENERATE_MASTER",
    "payload": {"jobId": "456"},
    "webhook": "https://example.com/callback"
  }'
```

### Health Check

```bash
curl http://localhost:8080/health
```

## Configuration

Edit `src/main/resources/application.properties`:

```properties
codeq.base-url=https://codeq.example.com
codeq.producer-token=${CODEQ_PRODUCER_TOKEN}
codeq.worker-token=${CODEQ_WORKER_TOKEN}
```

Or use environment variables:

```bash
export CODEQ_BASE_URL=https://codeq.example.com
export CODEQ_PRODUCER_TOKEN=your-token
export CODEQ_WORKER_TOKEN=your-token
./mvnw quarkus:dev
```

## Docker

Build image:

```bash
./mvnw package
docker build -f src/main/docker/Dockerfile.jvm -t codeq-quarkus .
```

Run:

```bash
docker run -p 8080:8080 \
  -e CODEQ_BASE_URL=http://codeq:8080 \
  -e CODEQ_PRODUCER_TOKEN=token \
  -e CODEQ_WORKER_TOKEN=token \
  codeq-quarkus
```

## Native Image Docker

Build:

```bash
./mvnw package -Pnative -Dquarkus.native.container-build=true
docker build -f src/main/docker/Dockerfile.native -t codeq-quarkus-native .
```

Run:

```bash
docker run -p 8080:8080 codeq-quarkus-native
```

Benefits:
- Startup: ~0.016s
- Memory: ~30MB RSS
- Image size: ~150MB

## Architecture

```
┌─────────────────────────────┐
│   Quarkus Application       │
│                             │
│  ┌────────────────────────┐ │
│  │   TaskResource         │ │  REST API
│  │   (JAX-RS)             │ │
│  └──────────┬─────────────┘ │
│             │               │
│  ┌──────────▼─────────────┐ │
│  │  TaskProducerService   │ │  Business Logic
│  └──────────┬─────────────┘ │
│             │               │
│  ┌──────────▼─────────────┐ │
│  │    CodeQClient         │ │  SDK
│  │    (CDI Bean)          │ │
│  └────────────────────────┘ │
│                             │
│  ┌────────────────────────┐ │
│  │    CodeQWorker         │ │  Background Worker
│  │    (@Scheduled)        │ │
│  └────────────────────────┘ │
└─────────────────────────────┘
```

## Performance

### JVM Mode
- Startup: ~1.5s
- Memory: ~100MB
- Throughput: ~10k req/s

### Native Mode
- Startup: ~0.016s
- Memory: ~30MB
- Throughput: ~8k req/s

## See Also

- [Quarkus Documentation](https://quarkus.io/guides/)
- [CodeQ Java Integration Guide](../../../docs/integrations/21-java-integration.md)
- [CodeQ SDK](../../../sdks/java/core/)
