# NestJS + CodeQ Integration Example

Complete NestJS example demonstrating CodeQ integration as both producer and worker.

## 📦 What's Included

This example demonstrates:
- **Producer Pattern**: REST API endpoints for creating tasks
- **Worker Pattern**: Background service with scheduled task processing
- **Modular Architecture**: Separate modules for tasks, workers, and CodeQ configuration
- **Heartbeat Management**: Automatic lease extension for long-running tasks
- **Error Handling**: NACK on failure with error reporting
- **TypeScript**: Full type safety with CodeQ SDK types

## 🏗️ Architecture

````
┌─────────────────────────────────────────────────┐
│           NestJS Application                    │
│                                                 │
│  ┌──────────────┐        ┌─────────────────┐   │
│  │   Producer   │        │     Worker      │   │
│  │  (REST API)  │        │  (Background)   │   │
│  └──────────────┘        └─────────────────┘   │
│         │                        │              │
│         └────────┬───────────────┘              │
│                  │                              │
│         ┌────────▼────────┐                     │
│         │  CodeQ Client   │                     │
│         │   (@codeq/sdk)  │                     │
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

- Node.js 18+ and npm/yarn/pnpm
- CodeQ server running (see [Local Development Guide](../../docs/22-local-development.md))
- Producer and worker authentication tokens

### 1. Install Dependencies

````bash
cd examples/nodejs/nestjs
npm install
````

### 2. Configure Environment

Copy `.env.example` to `.env` and configure:

````bash
cp .env.example .env
````

Edit `.env`:

````env
# Application
PORT=3000
NODE_ENV=development

# CodeQ Configuration
CODEQ_BASE_URL=http://localhost:8080
CODEQ_PRODUCER_TOKEN=your-producer-token
CODEQ_WORKER_TOKEN=your-worker-token
````

**Getting Tokens**: See [Authentication Guide](../../docs/09-security.md) for obtaining JWT tokens.

### 3. Start Development Server

````bash
npm run start:dev
````

The application starts on `http://localhost:3000` with hot reload enabled.

## 📚 Project Structure

````
src/
├── main.ts                     # Application bootstrap
├── app.module.ts               # Root module
├── config/
│   └── codeq.module.ts         # CodeQ client configuration
├── tasks/
│   ├── tasks.module.ts         # Tasks module
│   ├── tasks.controller.ts     # REST endpoints (Producer)
│   └── tasks.service.ts        # Task creation logic
└── workers/
    ├── workers.module.ts       # Workers module
    └── worker.service.ts       # Background worker (Consumer)
````

## 🔧 Usage Examples

### Creating Tasks (Producer)

#### 1. Create Basic Task

````bash
curl -X POST http://localhost:3000/api/tasks/master \
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
curl -X POST http://localhost:3000/api/tasks/with-webhook \
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
curl -X POST http://localhost:3000/api/tasks/delayed \
  -H "Content-Type: application/json" \
  -d '{
    "command": "GENERATE_MASTER",
    "payload": { "jobId": "job-789" },
    "delaySeconds": 60
  }'
````

Task is enqueued but not claimable until 60 seconds have passed.

### Processing Tasks (Worker)

The worker service (`WorkerService`) automatically:
1. **Polls** for available tasks every 5 seconds
2. **Claims** tasks with 120-second lease
3. **Sends heartbeats** every 30 seconds during processing
4. **Processes** task using command-specific logic
5. **Submits results** or **NACKs** on failure

#### Worker Output

````
[WorkerService] Claimed task: 01JGXY... (command: GENERATE_MASTER)
[WorkerService] Processing job: job-123
[WorkerService] Heartbeat sent for task: 01JGXY...
[WorkerService] Task completed: 01JGXY...
````

## 🔑 Key Components

### CodeQ Module (`config/codeq.module.ts`)

Configures and provides `CodeQClient` as a dependency:

````typescript
@Module({
  providers: [
    {
      provide: 'CODEQ_CLIENT',
      useFactory: () => {
        return new CodeQClient({
          baseUrl: process.env.CODEQ_BASE_URL,
          producerToken: process.env.CODEQ_PRODUCER_TOKEN,
          workerToken: process.env.CODEQ_WORKER_TOKEN,
          timeout: 30000,
          retries: 3,
        });
      },
    },
  ],
  exports: ['CODEQ_CLIENT'],
})
export class CodeQModule {}
````

### Tasks Controller (`tasks/tasks.controller.ts`)

REST endpoints for task creation:
- `POST /api/tasks/master` - Create task with priority
- `POST /api/tasks/with-webhook` - Create task with result webhook
- `POST /api/tasks/delayed` - Create delayed task

### Worker Service (`workers/worker.service.ts`)

Background worker with two scheduled jobs:

````typescript
@Cron(CronExpression.EVERY_5_SECONDS)
async pollAndProcess(): Promise<void> {
  const task = await this.codeQClient.claimTask({
    commands: ['GENERATE_MASTER', 'GENERATE_CREATIVE'],
    leaseSeconds: 120,
    waitSeconds: 10, // Long-polling
  });
  
  if (task) {
    await this.processTask(task);
  }
}

@Cron(CronExpression.EVERY_30_SECONDS)
async sendHeartbeat(): Promise<void> {
  if (this.currentTask) {
    await this.codeQClient.heartbeat(this.currentTask.id, 60);
  }
}
````

## 🎯 Best Practices

### 1. Long-Polling for Efficiency

````typescript
const task = await codeQClient.claimTask({
  commands: ['MY_COMMAND'],
  leaseSeconds: 120,
  waitSeconds: 10, // Wait up to 10s for task
});
````

Reduces unnecessary HTTP requests when queues are empty.

### 2. Heartbeat Management

Send heartbeats at 1/3 to 1/2 of the lease duration:
- Lease: 120 seconds → Heartbeat every 30-60 seconds
- Lease: 60 seconds → Heartbeat every 20-30 seconds

### 3. NACK on Transient Failures

````typescript
try {
  await processTask(task);
  await codeQClient.submitResult(task.id, { success: true });
} catch (error) {
  if (isTransientError(error)) {
    // Retry with exponential backoff
    await codeQClient.nack(task.id, {
      error: error.message,
      willRetry: true,
    });
  } else {
    // Permanent failure
    await codeQClient.submitResult(task.id, {
      success: false,
      error: error.message,
    });
  }
}
````

### 4. Graceful Shutdown

Use NestJS lifecycle hooks:

````typescript
async onModuleDestroy() {
  if (this.currentTask) {
    await this.codeQClient.abandon(this.currentTask.id);
    this.logger.log('Abandoned current task on shutdown');
  }
}
````

## 🧪 Testing

````bash
# Unit tests
npm run test

# E2E tests
npm run test:e2e
````

## 📊 Production Considerations

### Scaling Workers

Run multiple worker instances:
````bash
# Instance 1
PORT=3001 npm start

# Instance 2
PORT=3002 npm start
````

Each instance claims and processes tasks independently. CodeQ ensures no duplicate processing via lease mechanism.

### Monitoring

Enable NestJS built-in logger:
````typescript
const app = await NestFactory.create(AppModule, {
  logger: ['error', 'warn', 'log', 'debug', 'verbose'],
});
````

### Health Checks

Add a health check endpoint:
````typescript
@Controller('health')
export class HealthController {
  @Get()
  health() {
    return { status: 'ok', timestamp: new Date() };
  }
}
````

## 🔗 Related Documentation

- [CodeQ Getting Started](../../docs/00-getting-started.md)
- [Node.js Integration Guide](../../docs/integrations/nodejs-integration.md)
- [HTTP API Reference](../../docs/04-http-api.md)
- [SDK Documentation](../../sdks/README.md)
- [Local Development](../../docs/22-local-development.md)

## 🐛 Troubleshooting

### Issue: "Connection refused" when creating tasks

**Solution**: Ensure CodeQ server is running on `http://localhost:8080`:
````bash
cd ../..
docker compose up -d
````

### Issue: "Unauthorized" error

**Solution**: Verify your tokens are valid JWT tokens. See [Authentication Guide](../../docs/09-security.md).

### Issue: Worker not claiming tasks

**Solution**: Check:
1. Worker token has `codeq:claim` and `codeq:result` scopes
2. Task command is in the worker's `commands` array
3. Task is not already claimed by another worker

### Issue: Tasks timing out

**Solution**: Increase lease duration or send heartbeats more frequently:
````typescript
const task = await codeQClient.claimTask({
  commands: ['MY_COMMAND'],
  leaseSeconds: 300, // 5 minutes
});
````

## 📝 License

This example is part of the CodeQ project and is available under the same license.
