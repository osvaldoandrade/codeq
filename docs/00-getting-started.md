# Getting Started with codeQ

This tutorial will guide you through your first experience with codeQ, from installation to running a complete task workflow.

## What You'll Learn

By the end of this tutorial, you'll understand how to:

- Install the codeQ CLI
- Configure a local development environment
- Create and enqueue tasks
- Claim and process tasks as a worker
- Monitor queue status

## Prerequisites

- Basic familiarity with command-line interfaces
- Understanding of task queues and worker patterns
- For local development: Docker or access to a KVRocks instance

## Installation

### Option 1: Install via npm (Recommended)

````bash
npm install -g @osvaldoandrade/codeq
codeq --help
````

### Option 2: Install from source

````bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
````

Requires `git` and `go` to be installed.

### Verify Installation

````bash
codeq version
````

You should see the version information displayed.

## Setting Up Your Environment

### Quick Start with Docker Compose (Recommended)

The easiest way to get started is using Docker Compose, which sets up everything you need:

````bash
# Clone the repository
git clone https://github.com/osvaldoandrade/codeq
cd codeq

# Start all services (KVRocks + codeQ API + example tasks)
docker compose up -d

# Verify the server is running
curl -sSf http://localhost:8080/metrics | head
````

This will start:
- **KVRocks**: Redis-compatible storage on port 6666
- **codeQ API**: HTTP server on port 8080 with hot reload
- **Seed service**: Creates example tasks automatically

To view logs:

````bash
docker compose logs -f codeq
````

To stop all services:

````bash
docker compose down
````

For more details on local development with Docker Compose, including hot reload and observability stack, see [Local Development Guide](22-local-development.md).

### Alternative: Manual Setup

If you prefer manual setup or need more control:

#### Step 1: Start a Local KVRocks Instance

````bash
docker run -d -p 6666:6666 --name kvrocks apache/kvrocks:latest
````

#### Step 2: Configure codeQ Profile

````bash
codeq config init dev
codeq config set baseUrl http://localhost:8080
codeq config set redisAddr localhost:6666
````

#### Step 3: Start the codeQ Server

````bash
# From the repository root
go run ./cmd/server
````

The server will start on port 8080 by default.

## Your First Task Workflow

Now let's walk through a complete task lifecycle: create, claim, and complete.

### Step 1: Create a Task

First, let's create a simple task:

````bash
codeq task create \
  --command EXAMPLE_TASK \
  --payload '{"message": "Hello from codeQ"}' \
  --priority 5
````

You should see output similar to:

````json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "command": "EXAMPLE_TASK",
  "status": "READY",
  "priority": 5,
  "createdAt": "2026-02-14T20:30:00Z"
}
````

**Understanding the response:**
- `id`: Unique identifier for this task
- `command`: The task type/routing key
- `status`: Current state (READY, IN_PROGRESS, COMPLETED, FAILED)
- `priority`: Higher numbers = higher priority (0-10)

### Step 2: Check Queue Stats

Before claiming a task, let's check the queue status:

````bash
codeq queue stats --command EXAMPLE_TASK
````

Output:

````json
{
  "command": "EXAMPLE_TASK",
  "ready": 1,
  "delayed": 0,
  "inProgress": 0,
  "dlq": 0
}
````

This shows we have 1 task ready to be claimed.

### Step 3: Claim the Task

Now, let's act as a worker and claim the task:

````bash
codeq task claim \
  --commands EXAMPLE_TASK \
  --lease 120 \
  --wait 5
````

**Parameters explained:**
- `--commands`: Task types this worker can handle (comma-separated list)
- `--lease`: How long (in seconds) to hold the task before it expires
- `--wait`: How long to wait for a task if none are immediately available

Output:

````json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "command": "EXAMPLE_TASK",
  "payload": "{\"message\": \"Hello from codeQ\"}",
  "status": "IN_PROGRESS",
  "leaseExpiresAt": "2026-02-14T20:32:00Z"
}
````

The task is now assigned to you and will expire in 120 seconds if not completed.

### Step 4: Complete the Task

After processing the task, submit the result:

````bash
codeq task complete <TASK_ID> \
  --status COMPLETED \
  --result '{"processed": true, "output": "Task completed successfully"}'
````

Replace `<TASK_ID>` with the actual task ID from the claim response.

### Step 5: Verify Completion

Check the task status:

````bash
codeq task get <TASK_ID>
````

The status should show `COMPLETED`.

## Understanding Task States

Tasks in codeQ move through these states:

````
READY → IN_PROGRESS → COMPLETED
                    ↓
                  FAILED → DLQ (after max attempts)
                    ↑
                  NACK (retry)
````

**State descriptions:**

- **READY**: Task is in the queue, waiting to be claimed
- **IN_PROGRESS**: Task is claimed by a worker, lease is active
- **COMPLETED**: Task finished successfully
- **FAILED**: Task failed and will retry (if attempts remain)
- **DLQ**: Dead Letter Queue - task exhausted retry attempts

## Working with Delayed Tasks

Schedule a task to run in the future:

````bash
codeq task create \
  --command SCHEDULED_TASK \
  --payload '{"scheduled": true}' \
  --delay 60
````

Or specify an exact time:

````bash
codeq task create \
  --command SCHEDULED_TASK \
  --payload '{"scheduled": true}' \
  --run-at "2026-02-15T10:00:00Z"
````

Check delayed tasks:

````bash
codeq queue stats --command SCHEDULED_TASK
````

You'll see the task count in the `delayed` field.

## Handling Failures

If a task fails and needs to be retried, use NACK:

````bash
codeq task nack <TASK_ID> \
  --delay 30 \
  --reason "Temporary service unavailable"
````

This returns the task to the queue with a 30-second delay before it becomes available again.

## Monitoring Your Queues

View all queues:

````bash
codeq queue list
````

Get detailed statistics for a specific queue:

````bash
codeq queue stats --command EXAMPLE_TASK
````

## Next Steps

Now that you understand the basics, explore these topics:

- **[HTTP API](04-http-api.md)**: Learn to integrate with codeQ programmatically
- **[CLI Reference](15-cli-reference.md)**: Complete CLI command documentation
- **[Webhooks](12-webhooks.md)**: Set up push notifications for workers
- **[Configuration](14-configuration.md)**: Advanced configuration options
- **[Examples](13-examples.md)**: More usage patterns and examples

## Optional: Enable Distributed Tracing

codeQ supports OpenTelemetry distributed tracing to help you trace tasks through their entire lifecycle.

### Quick Setup (Local Development)

1. Start codeQ with the observability stack (includes Jaeger):

````bash
docker compose --profile obs up -d
````

2. Enable tracing in your `.env` file:

````bash
TRACING_ENABLED=true
TRACING_SERVICE_NAME=codeq
TRACING_OTLP_ENDPOINT=jaeger:4317
TRACING_OTLP_INSECURE=true
TRACING_SAMPLE_RATIO=1.0
````

3. Restart codeQ:

````bash
docker compose restart codeq
````

4. Create and process a task, then view traces in Jaeger UI:

````bash
# Open Jaeger UI
open http://localhost:16686

# Create a task
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{
    "command": "PROCESS_ORDER",
    "payload": {"orderId": "12345"}
  }'
````

### What Gets Traced

- HTTP requests (inbound and outbound)
- Task lifecycle (create → claim → process → complete)
- Webhook deliveries (worker notifications and result callbacks)
- W3C trace context propagation across service boundaries

See `docs/10-operations.md` for production tracing configuration and `docs/14-configuration.md` for all tracing options.

## Troubleshooting

### Task not being claimed

- Verify the command name matches exactly (case-sensitive)
- Check queue stats to confirm tasks are in READY state
- Ensure your worker lease duration is reasonable (60-300 seconds)

### Connection refused

- Verify the codeQ server is running
- Check your profile configuration: `codeq config show`
- Ensure KVRocks is accessible at the configured address

### Authentication errors

- codeQ uses JWT tokens for authentication in production
- See [Security](09-security.md) for authentication setup
- Local development may use mock tokens or no authentication depending on configuration

## Summary

You've successfully:

✓ Installed the codeQ CLI  
✓ Configured a local environment  
✓ Created and enqueued tasks  
✓ Claimed tasks as a worker  
✓ Completed tasks and monitored queues

Continue exploring the documentation to learn about advanced features like webhooks, priority queuing, and production deployment patterns.
