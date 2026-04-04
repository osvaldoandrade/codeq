# CodeQ Client SDKs and Integration Examples

This directory contains official SDKs and integration examples for CodeQ in multiple languages and frameworks.

## 📦 Available SDKs

### Go SDK
- **Location**: `sdks/go/`
- **Features**:
  - Idiomatic Go API with `context.Context` support
  - Functional options pattern for configuration
  - Zero external dependencies (stdlib `net/http` + `encoding/json`)
  - Full API coverage (tasks, subscriptions, admin, batch operations)
  - Automatic token-based authentication
  - Custom `*http.Client` support for advanced use cases
  - Convenience `WaitForResult` polling method

**Installation**:
```bash
go get github.com/osvaldoandrade/codeq/sdks/go
```

**Quick Start**:
```go
package main

import (
	"context"
	"fmt"
	"log"

	codeq "github.com/osvaldoandrade/codeq/sdks/go"
)

func main() {
	client := codeq.NewClient("https://codeq.example.com",
		codeq.WithProducerToken("your-producer-token"),
		codeq.WithWorkerToken("your-worker-token"),
	)

	ctx := context.Background()

	// Create a task
	task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
		Command:  "GENERATE_MASTER",
		Payload:  map[string]any{"jobId": "123"},
		Priority: codeq.Int(5),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Created task:", task.ID)

	// Claim a task
	claimed, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
		Commands:     []string{"GENERATE_MASTER"},
		LeaseSeconds: codeq.Int(120),
		WaitSeconds:  codeq.Int(10),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Submit result
	if claimed != nil {
		_, err = client.SubmitResult(ctx, claimed.ID, &codeq.SubmitResultOptions{
			Status: "COMPLETED",
			Result: map[string]any{"success": true},
		})
		if err != nil {
			log.Fatal(err)
		}
	}
}
```

### Java SDK
- **Location**: `sdks/java/core/`
- **Frameworks**: Spring Boot, Quarkus, Micronaut
- **Features**:
  - Type-safe API with full Java 17+ support
  - OkHttp-based HTTP client with connection pooling
  - Jackson for JSON serialization
  - JWT authentication support
  - Comprehensive error handling

**Installation**:
```xml
<dependency>
    <groupId>io.codeq</groupId>
    <artifactId>codeq-sdk-java</artifactId>
    <version>1.0.0</version>
</dependency>
```

**Quick Start**:
```java
CodeQClient client = CodeQClient.builder()
    .baseUrl("https://codeq.example.com")
    .producerToken("your-producer-token")
    .workerToken("your-worker-token")
    .build();

// Create a task
Task task = client.createTask("GENERATE_MASTER", Map.of("jobId", "123"), 5);

// Claim a task
Task claimed = client.claimTask(List.of("GENERATE_MASTER"), 120, 10);

// Submit result
client.submitResult(claimed.getId(), Map.of("status", "success"));
```

### Python SDK
- **Location**: `sdks/python/`
- **Frameworks**: FastAPI, Django, Flask, asyncio
- **Features**:
  - Full type hints with PEP 561 marker
  - Async (`CodeQClient`) and sync (`SyncCodeQClient`) variants
  - httpx-based HTTP client with connection pooling
  - Automatic retry with exponential backoff (tenacity)
  - Comprehensive docstrings
  - Full API coverage (tasks, subscriptions, admin)

**Installation**:
```bash
pip install codeq-client
```

**Quick Start**:
```python
from codeq import CodeQClient, CreateTaskOptions

async with CodeQClient(
    base_url="https://codeq.example.com",
    producer_token="your-producer-token",
    worker_token="your-worker-token",
) as client:
    # Create a task
    task = await client.create_task(
        CreateTaskOptions(
            command="GENERATE_MASTER",
            payload={"jobId": "123"},
            priority=5,
        )
    )

    # Claim a task
    from codeq import ClaimTaskOptions
    claimed = await client.claim_task(
        ClaimTaskOptions(
            commands=["GENERATE_MASTER"],
            lease_seconds=120,
            wait_seconds=10,
        )
    )

    # Submit result
    if claimed:
        from codeq import SubmitResultOptions
        await client.submit_result(
            claimed.id,
            SubmitResultOptions(status="COMPLETED", result={"success": True}),
        )
```

### Node.js/TypeScript SDK
- **Location**: `sdks/nodejs/`
- **Frameworks**: Express, NestJS, React
- **Features**:
  - Full TypeScript support with type definitions
  - Promise-based async API
  - Axios with automatic retry logic
  - Modern ES2020+ syntax
  - ESM and CJS builds
  - Full API coverage (tasks, subscriptions, admin)

**Installation**:
```bash
npm install @osvaldoandrade/codeq-client
```

**Quick Start**:
```typescript
import { CodeQClient } from '@osvaldoandrade/codeq-client';

const client = new CodeQClient({
  baseUrl: 'https://codeq.example.com',
  producerToken: 'your-producer-token',
  workerToken: 'your-worker-token',
});

// Create a task
const task = await client.createTask({
  command: 'GENERATE_MASTER',
  payload: { jobId: '123' },
  priority: 5,
});

// Claim a task
const claimed = await client.claimTask({
  commands: ['GENERATE_MASTER'],
  leaseSeconds: 120,
  waitSeconds: 10,
});

// Submit result
if (claimed) {
  await client.submitResult(claimed.id, {
    status: 'COMPLETED',
    result: { success: true },
  });
}
```

## 🚀 Integration Examples

### Java Examples

#### Spring Boot
- **Location**: `examples/java/springboot/`
- **Features**:
  - REST API for task creation
  - Background worker with `@Scheduled`
  - Dependency injection with Spring beans
  - Actuator health checks
  - Graceful shutdown handling

**Run**:
```bash
cd examples/java/springboot
mvn spring-boot:run
```

#### Quarkus
- **Location**: `examples/java/quarkus/`
- **Features**:
  - Reactive programming model
  - Native image support
  - Fast startup time
  - Low memory footprint

**Run**:
```bash
cd examples/java/quarkus
./mvnw quarkus:dev
```

### Node.js/TypeScript Examples

#### NestJS
- **Location**: `examples/nodejs/nestjs/`
- **Features**:
  - Modular architecture
  - Dependency injection
  - Decorators for scheduling
  - Built-in validation

**Run**:
```bash
cd examples/nodejs/nestjs
npm install
npm run start:dev
```

## 📚 Documentation

### Integration Guides
- [Java Integration Guide](../docs/integrations/java-integration.md)
- [Node.js/TypeScript Integration Guide](../docs/integrations/nodejs-integration.md)
- [Python Integration Guide](../docs/integrations/python-integration.md)

### Deployment Recipes
- [Kubernetes Deployment](deploy/kubernetes/)
  - Spring Boot deployment with HPA
  - NestJS deployment with autoscaling
  - Ingress configuration
- [Docker Compose](deploy/docker-compose/)
  - Complete stack with CodeQ + KVRocks
  - Multi-service setup
  - Nginx reverse proxy

### Core Documentation
- [Getting Started](docs/00-getting-started.md)
- [HTTP API Reference](docs/04-http-api.md)
- [Configuration Guide](docs/14-configuration.md)
- [Performance Tuning](docs/17-performance-tuning.md)

## 🏗️ Architecture Patterns

### Producer Pattern
Microservices that create tasks:
```
┌─────────────────┐
│  Microservice   │
│  (Producer)     │
│                 │
│  ┌───────────┐  │
│  │ Business  │  │
│  │  Logic    │  │
│  └─────┬─────┘  │
│        │        │
│  ┌─────▼─────┐  │
│  │  CodeQ    │  │
│  │  Client   │  │
│  └─────┬─────┘  │
└────────┼────────┘
         │ HTTP
         ▼
   ┌─────────────┐
   │   CodeQ     │
   │   Server    │
   └─────────────┘
```

### Worker Pattern
Microservices that process tasks:
```
┌─────────────────┐
│  Microservice   │
│  (Worker)       │
│                 │
│  ┌───────────┐  │
│  │  Worker   │  │
│  │  Service  │  │
│  └─────┬─────┘  │
│        │        │
│  ┌─────▼─────┐  │
│  │  CodeQ    │  │
│  │  Client   │  │
│  └─────┬─────┘  │
└────────┼────────┘
         │ HTTP
         ▼
   ┌─────────────┐
   │   CodeQ     │
   │   Server    │
   └─────────────┘
```

### Hybrid Pattern
Microservices that both produce and consume:
```
┌─────────────────────┐
│   Microservice      │
│  (Producer+Worker)  │
│                     │
│  ┌──────────────┐   │
│  │   REST API   │   │
│  │  (Producer)  │   │
│  └──────┬───────┘   │
│         │           │
│  ┌──────▼───────┐   │
│  │    CodeQ     │   │
│  │    Client    │   │
│  └──────▲───────┘   │
│         │           │
│  ┌──────┴───────┐   │
│  │   Worker     │   │
│  │   Service    │   │
│  └──────────────┘   │
└─────────────────────┘
```

## 🔧 Configuration

### Environment Variables

All examples support these environment variables:

```bash
# CodeQ Server
CODEQ_BASE_URL=https://codeq.example.com

# Authentication
CODEQ_PRODUCER_TOKEN=your-producer-token
CODEQ_WORKER_TOKEN=your-worker-token

# Optional: Timeouts
CODEQ_TIMEOUT_MS=30000
CODEQ_RETRIES=3
```

### Framework-Specific Configuration

**Spring Boot** (`application.properties`):
```properties
codeq.base-url=${CODEQ_BASE_URL}
codeq.producer-token=${CODEQ_PRODUCER_TOKEN}
codeq.worker-token=${CODEQ_WORKER_TOKEN}
```

**NestJS** (`.env`):
```bash
CODEQ_BASE_URL=https://codeq.example.com
CODEQ_PRODUCER_TOKEN=your-producer-token
CODEQ_WORKER_TOKEN=your-worker-token
```

**Express** (`.env`):
```bash
CODEQ_BASE_URL=https://codeq.example.com
CODEQ_PRODUCER_TOKEN=your-producer-token
CODEQ_WORKER_TOKEN=your-worker-token
```

## 🧪 Testing

### Unit Tests

**Go**:
```bash
cd sdks/go
go test ./... -v
```

**Python**:
```bash
cd sdks/python
pip install -e ".[dev]"
pytest --cov
```

**Java**:
```bash
cd sdks/java/core
mvn test
```

**Node.js**:
```bash
cd sdks/nodejs
npm test
```

### Integration Tests

Run with local CodeQ server:

```bash
# Start CodeQ with docker-compose
cd deploy/docker-compose
docker-compose up -d

# Run integration tests
cd examples/java/springboot
mvn verify

cd examples/nodejs/nestjs
npm run test:e2e
```

## 🚢 Deployment

### Kubernetes

Deploy Spring Boot example:
```bash
kubectl apply -f deploy/kubernetes/springboot-deployment.yaml
```

Deploy NestJS example:
```bash
kubectl apply -f deploy/kubernetes/nestjs-deployment.yaml
```

### Docker Compose

Run complete stack:
```bash
cd deploy/docker-compose
docker-compose up -d
```

Access services:
- CodeQ Server: http://localhost:8080
- Spring Boot: http://localhost:8081
- NestJS: http://localhost:3000
- Express: http://localhost:3001

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on:
- Adding new SDK features
- Creating framework examples
- Writing documentation
- Submitting pull requests

## 📄 License

MIT License - see [LICENSE](LICENSE) for details.

## 🔗 Links

- [GitHub Repository](https://github.com/osvaldoandrade/codeq)
- [Documentation](https://github.com/osvaldoandrade/codeq/tree/main/docs)
- [Issues](https://github.com/osvaldoandrade/codeq/issues)
- [Discussions](https://github.com/osvaldoandrade/codeq/discussions)
