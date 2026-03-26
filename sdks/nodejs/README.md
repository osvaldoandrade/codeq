# @osvaldoandrade/codeq-client

Official TypeScript/JavaScript SDK for the [CodeQ](https://github.com/osvaldoandrade/codeq) reactive task scheduling system. Works in Node.js and browsers.

## Features

- **Full API coverage** — Task creation, claiming, results, webhooks, and admin operations
- **TypeScript first** — Complete type definitions with comprehensive JSDoc
- **Dual module builds** — ESM and CommonJS outputs
- **Automatic retry** — Exponential backoff on transient failures (via axios-retry)
- **Promise-based** — Modern async/await API
- **Lightweight** — Only depends on axios

## Installation

```bash
npm install @osvaldoandrade/codeq-client
```

## Quick Start

### Producer — Create tasks

```typescript
import { CodeQClient } from '@osvaldoandrade/codeq-client';

const client = new CodeQClient({
  baseUrl: 'http://localhost:8080',
  producerToken: 'your-producer-jwt',
});

const task = await client.createTask({
  command: 'GENERATE_MASTER',
  payload: { jobId: 'j-123' },
  priority: 5,
});

console.log('Created task:', task.id);
```

### Worker — Claim and process tasks

```typescript
import { CodeQClient } from '@osvaldoandrade/codeq-client';

const client = new CodeQClient({
  baseUrl: 'http://localhost:8080',
  workerToken: 'your-worker-jwt',
});

// Long-poll for a task
const task = await client.claimTask({
  commands: ['GENERATE_MASTER'],
  leaseSeconds: 120,
  waitSeconds: 10,
});

if (task) {
  // Send periodic heartbeats
  const heartbeatInterval = setInterval(
    () => client.heartbeat(task.id, 120),
    60_000
  );

  try {
    const output = await doWork(task.payload);
    await client.submitResult(task.id, {
      status: 'COMPLETED',
      result: output,
    });
  } catch (err: any) {
    await client.nack(task.id, 30, err.message);
  } finally {
    clearInterval(heartbeatInterval);
  }
}
```

### Wait for result

```typescript
const task = await client.createTask({
  command: 'GENERATE_MASTER',
  payload: { jobId: 'j-123' },
  priority: 5,
});

const { result } = await client.waitForResult(task.id, {
  timeout: 60_000,
  pollInterval: 2_000,
});

console.log(result.status, result.result);
```

### Webhook subscriptions

```typescript
const sub = await client.createSubscription({
  callbackUrl: 'https://myapp.example.com/webhook',
  eventTypes: ['GENERATE_MASTER'],
  ttlSeconds: 3600,
  deliveryMode: 'group',
  groupId: 'worker-pool-1',
});

console.log('Subscription:', sub.subscriptionId, 'expires:', sub.expiresAt);

// Renew before expiration
await client.renewSubscription(sub.subscriptionId, { ttlSeconds: 3600 });
```

### Admin operations

```typescript
const admin = new CodeQClient({
  baseUrl: 'http://localhost:8080',
  adminToken: 'your-admin-jwt',
});

// List all queues
const queues = await admin.listQueues();
queues.forEach((q) => console.log(q.command, q.ready, q.inProgress));

// Get stats for a specific command
const stats = await admin.getQueueStats('GENERATE_MASTER');

// Cleanup expired tasks
const cleanup = await admin.cleanupExpired({ limit: 500 });
console.log('Deleted:', cleanup.deleted);
```

## API Reference

### `CodeQClient(config)`

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `baseUrl` | `string` | — | CodeQ server URL |
| `producerToken` | `string?` | — | JWT for producer operations |
| `workerToken` | `string?` | — | JWT for worker operations |
| `adminToken` | `string?` | — | JWT for admin operations |
| `timeout` | `number?` | `30000` | Request timeout (ms) |
| `retries` | `number?` | `3` | Automatic retry count |

### Producer Methods

| Method | Description |
|--------|-------------|
| `createTask(options)` | Create a new task |

### Worker Methods

| Method | Description |
|--------|-------------|
| `claimTask(options)` | Claim a task (returns `null` if none available) |
| `submitResult(taskId, options)` | Submit a completed/failed result |
| `heartbeat(taskId, extendSeconds?)` | Extend task lease |
| `abandon(taskId)` | Return task to queue |
| `nack(taskId, delaySeconds, reason)` | Negative ack with retry delay |
| `createSubscription(options)` | Register webhook subscription |
| `renewSubscription(subscriptionId, options?)` | Renew subscription TTL |

### Query Methods

| Method | Description |
|--------|-------------|
| `getTask(taskId)` | Get task by ID |
| `getResult(taskId)` | Get task result |
| `waitForResult(taskId, options?)` | Poll until result is available |

### Admin Methods

| Method | Description |
|--------|-------------|
| `listQueues()` | List all queue statistics |
| `getQueueStats(command)` | Get stats for a specific queue |
| `cleanupExpired(options?)` | Remove expired tasks |

## Types

All TypeScript types are exported from the package:

```typescript
import {
  CodeQClient,
  CodeQClientConfig,
  Task,
  TaskStatus,
  CreateTaskOptions,
  ClaimTaskOptions,
  SubmitResultOptions,
  ResultRecord,
  TaskResult,
  NackResponse,
  ArtifactIn,
  ArtifactOut,
  QueueStats,
  CreateSubscriptionOptions,
  SubscriptionResponse,
  RenewSubscriptionOptions,
  WaitForResultOptions,
  CleanupOptions,
  CleanupResult,
} from '@osvaldoandrade/codeq-client';
```

## Browser Usage

The SDK works in browsers that support ES2020. Ensure your CodeQ server has CORS configured for your origin.

```typescript
// ESM import in browser bundlers (Vite, webpack, etc.)
import { CodeQClient } from '@osvaldoandrade/codeq-client';

const client = new CodeQClient({
  baseUrl: 'https://codeq.example.com',
  producerToken: 'your-token',
});
```

## Development

```bash
# Install dependencies
npm install

# Build (CJS + ESM)
npm run build

# Run tests with coverage
npm test

# Lint
npm run lint
```

## License

MIT
