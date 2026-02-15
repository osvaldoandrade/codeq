# CodeQ Examples

This directory contains working example applications demonstrating how to integrate CodeQ with popular frameworks and languages.

## 📁 Structure

````
examples/
├── java/          # Java framework examples
│   ├── springboot/    # Spring Boot integration
│   └── quarkus/       # Quarkus integration
└── nodejs/        # Node.js/TypeScript framework examples
    └── nestjs/        # NestJS integration
````

## 🚀 Java Examples

### Spring Boot
**Location**: `java/springboot/`

A complete Spring Boot application demonstrating:
- Task creation via REST API
- Background worker with `@Scheduled` polling
- Dependency injection for CodeQ client
- Health checks with Spring Actuator
- Graceful shutdown handling

**Quick Start**:
````bash
cd java/springboot
mvn spring-boot:run
````

**Documentation**: See [java/springboot/README.md](java/springboot/README.md)

---

### Quarkus
**Location**: `java/quarkus/`

A Quarkus application showcasing:
- Reactive programming with CodeQ
- CDI for dependency injection
- Fast startup and low memory footprint
- Native image compilation support

**Quick Start**:
````bash
cd java/quarkus
./mvnw quarkus:dev
````

**Documentation**: See [java/quarkus/README.md](java/quarkus/README.md)

---

## 🟢 Node.js/TypeScript Examples

### NestJS
**Location**: `nodejs/nestjs/`

A production-ready NestJS application featuring:
- Modular architecture with CodeQ module
- Task creation via REST controllers
- Background worker with cron scheduling
- TypeScript with full type safety
- Environment-based configuration

**Quick Start**:
````bash
cd nodejs/nestjs
npm install
npm run start:dev
````

**Documentation**: See inline code comments

---

## 🔧 Prerequisites

### All Examples
- Docker and Docker Compose (for running CodeQ server)
- Git

### Java Examples
- Java 17 or higher
- Maven 3.8+ or Gradle 7+

### Node.js Examples
- Node.js 18 or higher
- npm, yarn, or pnpm

## 🏃 Running Examples

### 1. Start CodeQ Server

All examples require a running CodeQ server. Use Docker Compose:

````bash
cd ../deploy/docker-compose
docker-compose up -d
````

This starts:
- CodeQ server on `http://localhost:8080`
- KVRocks (Redis) on `localhost:6666`

### 2. Configure Authentication

Each example requires producer and worker tokens. Set environment variables:

````bash
export CODEQ_BASE_URL=http://localhost:8080
export CODEQ_PRODUCER_TOKEN=your-producer-token
export CODEQ_WORKER_TOKEN=your-worker-token
````

Or create a `.env` file in each example directory.

### 3. Run an Example

Choose an example and follow its Quick Start instructions above.

## 📚 Learning Path

**New to CodeQ?** Follow this path:

1. **Read**: [Getting Started Guide](../docs/00-getting-started.md)
2. **Choose your stack**:
   - Java → Start with [Spring Boot example](java/springboot/)
   - Node.js → Start with [NestJS example](nodejs/nestjs/)
3. **Explore**: [Integration Guides](../docs/integrations/)
   - [Java Integration Guide](../docs/integrations/21-java-integration.md)
   - [Node.js Integration Guide](../docs/integrations/22-nodejs-integration.md)

## 🏗️ Architecture Patterns

### Producer Pattern
Create tasks from your business logic:

````java
// Java
Task task = codeQClient.createTask("PROCESS_ORDER", payload, priority);
````

````typescript
// TypeScript
const task = await codeQClient.createTask({
  command: 'PROCESS_ORDER',
  payload,
  priority: 5
});
````

### Worker Pattern
Process tasks in the background:

````java
// Java - Scheduled polling
@Scheduled(fixedDelay = 5000)
public void pollTasks() {
    Task task = codeQClient.claimTask(commands, 120, 10);
    if (task != null) processTask(task);
}
````

````typescript
// TypeScript - Cron-based polling
@Cron(CronExpression.EVERY_5_SECONDS)
async pollTasks() {
  const task = await codeQClient.claimTask({
    commands: ['PROCESS_ORDER'],
    leaseSeconds: 120,
    waitSeconds: 10
  });
  if (task) await this.processTask(task);
}
````

### Hybrid Pattern
Both produce and consume tasks in a single service. See examples for complete implementations.

## 🧪 Testing Examples

Each example includes its own test suite. Run tests:

````bash
# Java (Spring Boot)
cd java/springboot
mvn test

# Java (Quarkus)
cd java/quarkus
./mvnw test

# Node.js (NestJS)
cd nodejs/nestjs
npm test
````

## 📖 Additional Resources

- **SDK Documentation**: [sdks/README.md](../sdks/README.md)
- **HTTP API Reference**: [docs/04-http-api.md](../docs/04-http-api.md)
- **Configuration Guide**: [docs/14-configuration.md](../docs/14-configuration.md)
- **Performance Tuning**: [docs/17-performance-tuning.md](../docs/17-performance-tuning.md)

## 🤝 Contributing

Want to add an example? See [CONTRIBUTING.md](../CONTRIBUTING.md) for guidelines on:
- Adding new framework examples
- Improving existing examples
- Documentation best practices

## 📄 License

All examples are MIT licensed - see [LICENSE](../LICENSE) for details.
