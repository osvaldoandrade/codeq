# Node.js/TypeScript Integration Guide

Complete guide for integrating CodeQ with Node.js/TypeScript microservices using Express, NestJS, and React.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [Express Integration](#express-integration)
- [NestJS Integration](#nestjs-integration)
- [React Integration](#react-integration)
- [Best Practices](#best-practices)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Node.js SDK provides a modern, Promise-based API with full TypeScript support for:
- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations

### Architecture

```
┌─────────────────┐         ┌─────────────┐         ┌──────────────┐
│  Express/Nest   │────────▶│   CodeQ     │◀────────│   Worker     │
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

### npm

```bash
npm install @codeq/sdk
```

### yarn

```bash
yarn add @codeq/sdk
```

### pnpm

```bash
pnpm add @codeq/sdk
```

## Express Integration

### 1. Setup

Create `src/config/codeq.ts`:

```typescript
import { CodeQClient } from '@codeq/sdk';

export const codeQClient = new CodeQClient({
  baseUrl: process.env.CODEQ_BASE_URL || 'http://localhost:8080',
  producerToken: process.env.CODEQ_PRODUCER_TOKEN,
  workerToken: process.env.CODEQ_WORKER_TOKEN,
  timeout: 30000,
  retries: 3,
});
```

### 2. Producer Routes

Create `src/routes/tasks.ts`:

```typescript
import express from 'express';
import { codeQClient } from '../config/codeq';

const router = express.Router();

// Create a task
router.post('/tasks', async (req, res) => {
  try {
    const { command, payload, priority } = req.body;

    const task = await codeQClient.createTask({
      command,
      payload,
      priority: priority || 5,
    });

    res.json(task);
  } catch (error) {
    console.error('Failed to create task:', error);
    res.status(500).json({ error: 'Failed to create task' });
  }
});

// Create task with webhook
router.post('/tasks/with-webhook', async (req, res) => {
  try {
    const { command, payload, webhook } = req.body;

    const task = await codeQClient.createTask({
      command,
      payload,
      priority: 5,
      webhook,
      maxAttempts: 3,
    });

    res.json(task);
  } catch (error) {
    console.error('Failed to create task:', error);
    res.status(500).json({ error: 'Failed to create task' });
  }
});

export default router;
```

### 3. Worker Service

Create `src/workers/task-worker.ts`:

```typescript
import { codeQClient } from '../config/codeq';
import { Task } from '@codeq/sdk';

class TaskWorker {
  private currentTask: Task | null = null;
  private isRunning = false;

  async start() {
    this.isRunning = true;
    console.log('Worker started');

    // Start polling
    this.poll();

    // Start heartbeat
    this.startHeartbeat();
  }

  async stop() {
    this.isRunning = false;

    if (this.currentTask) {
      await codeQClient.abandon(this.currentTask.id);
      console.log('Abandoned task on shutdown');
    }
  }

  private async poll() {
    while (this.isRunning) {
      if (this.currentTask) {
        await this.sleep(5000);
        continue;
      }

      try {
        const task = await codeQClient.claimTask({
          commands: ['PROCESS_ORDER', 'SEND_EMAIL'],
          leaseSeconds: 120,
          waitSeconds: 10,
        });

        if (task) {
          this.currentTask = task;
          console.log(`Claimed task: ${task.id}`);
          await this.processTask(task);
        }
      } catch (error) {
        console.error('Error claiming task:', error);
        await this.sleep(5000);
      }
    }
  }

  private startHeartbeat() {
    setInterval(async () => {
      if (this.currentTask) {
        try {
          await codeQClient.heartbeat(this.currentTask.id, 60);
          console.log(`Heartbeat sent for task: ${this.currentTask.id}`);
        } catch (error) {
          console.error('Heartbeat failed:', error);
        }
      }
    }, 30000);
  }

  private async processTask(task: Task) {
    try {
      console.log(`Processing task: ${task.id}`, task.payload);

      let result: Record<string, any>;

      switch (task.command) {
        case 'PROCESS_ORDER':
          result = await this.processOrder(task);
          break;
        case 'SEND_EMAIL':
          result = await this.sendEmail(task);
          break;
        default:
          throw new Error(`Unknown command: ${task.command}`);
      }

      await codeQClient.submitResult(task.id, {
        status: 'COMPLETED',
        result,
      });

      console.log(`Task completed: ${task.id}`);
    } catch (error: any) {
      console.error(`Error processing task: ${task.id}`, error);
      await this.handleError(task, error);
    } finally {
      this.currentTask = null;
    }
  }

  private async processOrder(task: Task): Promise<Record<string, any>> {
    const { orderId } = task.payload;
    console.log(`Processing order: ${orderId}`);

    // Simulate work
    await this.sleep(5000);

    return {
      orderId,
      status: 'processed',
      processedAt: Date.now(),
    };
  }

  private async sendEmail(task: Task): Promise<Record<string, any>> {
    const { to, subject } = task.payload;
    console.log(`Sending email to: ${to}`);

    // Simulate work
    await this.sleep(2000);

    return {
      to,
      subject,
      sentAt: Date.now(),
    };
  }

  private async handleError(task: Task, error: any) {
    try {
      if (this.isRetryable(error)) {
        const delaySeconds = this.calculateBackoff(task.attempts || 0);
        await codeQClient.nack(task.id, delaySeconds, error.message);
        console.log(`Task NACKed: ${task.id} (delay: ${delaySeconds}s)`);
      } else {
        await codeQClient.submitResult(task.id, {
          status: 'FAILED',
          result: { error: error.message },
          error: error.message,
        });
        console.log(`Task failed permanently: ${task.id}`);
      }
    } catch (err) {
      console.error('Error handling task failure:', err);
    }
  }

  private isRetryable(error: any): boolean {
    return error.name !== 'ValidationError';
  }

  private calculateBackoff(attempts: number): number {
    return Math.min(300, Math.pow(2, attempts) * 5);
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}

export const taskWorker = new TaskWorker();
```

### 4. Main Application

Create `src/index.ts`:

```typescript
import express from 'express';
import taskRoutes from './routes/tasks';
import { taskWorker } from './workers/task-worker';

const app = express();
app.use(express.json());

// Routes
app.use('/api', taskRoutes);

// Health check
app.get('/health', (req, res) => {
  res.json({ status: 'ok' });
});

const PORT = process.env.PORT || 3000;

app.listen(PORT, () => {
  console.log(`Server running on port ${PORT}`);
  
  // Start worker
  taskWorker.start();
});

// Graceful shutdown
process.on('SIGTERM', async () => {
  console.log('SIGTERM received, shutting down gracefully');
  await taskWorker.stop();
  process.exit(0);
});
```

## NestJS Integration

See complete example in `examples/nodejs/nestjs/`.

### Key Features

- **Dependency Injection**: CodeQ client as injectable service
- **Decorators**: Use `@Cron` for worker scheduling
- **Modules**: Separate modules for producers and workers
- **Configuration**: Environment-based configuration with `@nestjs/config`

### Quick Start

```typescript
// Module
@Module({
  providers: [
    {
      provide: 'CODEQ_CLIENT',
      useFactory: (config: ConfigService) => {
        return new CodeQClient({
          baseUrl: config.get('CODEQ_BASE_URL'),
          producerToken: config.get('CODEQ_PRODUCER_TOKEN'),
          workerToken: config.get('CODEQ_WORKER_TOKEN'),
        });
      },
      inject: [ConfigService],
    },
  ],
  exports: ['CODEQ_CLIENT'],
})
export class CodeQModule {}

// Service
@Injectable()
export class TasksService {
  constructor(
    @Inject('CODEQ_CLIENT')
    private readonly codeQClient: CodeQClient,
  ) {}

  async createTask(options: CreateTaskOptions) {
    return this.codeQClient.createTask(options);
  }
}

// Worker
@Injectable()
export class WorkerService {
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
}
```

## React Integration

### 1. API Client Hook

Create `src/hooks/useCodeQ.ts`:

```typescript
import { useState, useCallback } from 'react';
import { CodeQClient, Task, CreateTaskOptions } from '@codeq/sdk';

const codeQClient = new CodeQClient({
  baseUrl: process.env.REACT_APP_CODEQ_BASE_URL || 'http://localhost:8080',
  producerToken: process.env.REACT_APP_CODEQ_PRODUCER_TOKEN,
});

export function useCodeQ() {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const createTask = useCallback(async (options: CreateTaskOptions): Promise<Task | null> => {
    setLoading(true);
    setError(null);

    try {
      const task = await codeQClient.createTask(options);
      return task;
    } catch (err: any) {
      setError(err);
      return null;
    } finally {
      setLoading(false);
    }
  }, []);

  const getTask = useCallback(async (taskId: string): Promise<Task | null> => {
    setLoading(true);
    setError(null);

    try {
      const task = await codeQClient.getTask(taskId);
      return task;
    } catch (err: any) {
      setError(err);
      return null;
    } finally {
      setLoading(false);
    }
  }, []);

  return { createTask, getTask, loading, error };
}
```

### 2. Component Example

```typescript
import React, { useState } from 'react';
import { useCodeQ } from './hooks/useCodeQ';

export function OrderForm() {
  const { createTask, loading, error } = useCodeQ();
  const [orderId, setOrderId] = useState('');
  const [taskId, setTaskId] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    const task = await createTask({
      command: 'PROCESS_ORDER',
      payload: { orderId },
      priority: 5,
    });

    if (task) {
      setTaskId(task.id);
      alert(`Task created: ${task.id}`);
    }
  };

  return (
    <form onSubmit={handleSubmit}>
      <input
        type="text"
        value={orderId}
        onChange={(e) => setOrderId(e.target.value)}
        placeholder="Order ID"
      />
      <button type="submit" disabled={loading}>
        {loading ? 'Creating...' : 'Create Task'}
      </button>
      {error && <div>Error: {error.message}</div>}
      {taskId && <div>Task ID: {taskId}</div>}
    </form>
  );
}
```

## Best Practices

### 1. Environment Variables

Use `.env` files:

```bash
# .env
CODEQ_BASE_URL=https://codeq.example.com
CODEQ_PRODUCER_TOKEN=your-producer-token
CODEQ_WORKER_TOKEN=your-worker-token
```

Load with `dotenv`:

```typescript
import 'dotenv/config';
```

### 2. Error Handling

Always handle errors:

```typescript
try {
  const task = await codeQClient.createTask(options);
} catch (error) {
  console.error('Failed to create task:', error);
  
  // Retry logic
  await retryWithBackoff(() => codeQClient.createTask(options));
  
  // Or queue locally
  await localQueue.add(options);
}
```

### 3. Graceful Shutdown

Handle SIGTERM:

```typescript
process.on('SIGTERM', async () => {
  console.log('Shutting down gracefully');
  
  if (currentTask) {
    await codeQClient.abandon(currentTask.id);
  }
  
  process.exit(0);
});
```

### 4. Monitoring

Add logging and metrics:

```typescript
import pino from 'pino';

const logger = pino();

async function processTask(task: Task) {
  const start = Date.now();
  
  try {
    // Process task
    const result = await doWork(task);
    
    logger.info({
      taskId: task.id,
      command: task.command,
      duration: Date.now() - start,
    }, 'Task completed');
    
    return result;
  } catch (error) {
    logger.error({
      taskId: task.id,
      error,
    }, 'Task failed');
    throw error;
  }
}
```

### 5. TypeScript Types

Use strong typing:

```typescript
interface OrderPayload {
  orderId: string;
  customerId: string;
  items: Array<{ sku: string; quantity: number }>;
}

interface OrderResult {
  orderId: string;
  status: 'processed' | 'failed';
  processedAt: number;
}

async function processOrder(task: Task): Promise<OrderResult> {
  const payload = task.payload as OrderPayload;
  
  // Type-safe processing
  const result: OrderResult = {
    orderId: payload.orderId,
    status: 'processed',
    processedAt: Date.now(),
  };
  
  return result;
}
```

## Troubleshooting

### Connection Issues

Check network connectivity:

```typescript
try {
  const task = await codeQClient.createTask(options);
} catch (error: any) {
  if (error.code === 'ECONNREFUSED') {
    console.error('Cannot connect to CodeQ server');
  } else if (error.code === 'ETIMEDOUT') {
    console.error('Connection timeout');
  }
}
```

### Authentication Errors

Verify tokens:

```bash
# Test producer token
curl -H "Authorization: Bearer $CODEQ_PRODUCER_TOKEN" \
  https://codeq.example.com/v1/codeq/tasks

# Test worker token
curl -H "Authorization: Bearer $CODEQ_WORKER_TOKEN" \
  https://codeq.example.com/v1/codeq/tasks/claim
```

### Memory Leaks

Monitor event listeners:

```typescript
// Avoid creating multiple clients
const codeQClient = new CodeQClient(config); // Singleton

// Clean up intervals
const heartbeatInterval = setInterval(() => {
  // Send heartbeat
}, 30000);

process.on('SIGTERM', () => {
  clearInterval(heartbeatInterval);
});
```

## See Also

- [HTTP API Reference](04-http-api.md)
- [Configuration Guide](14-configuration.md)
- [Performance Tuning](18-performance-tuning.md)
- [Example: Express](../examples/nodejs/express/)
- [Example: NestJS](../examples/nodejs/nestjs/)
- [Example: React](../examples/nodejs/react/)
