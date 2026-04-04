# codeq-client

Official Python SDK for the [CodeQ](https://github.com/osvaldoandrade/codeq) reactive task scheduling system. Provides both async and sync clients.

## Features

- **Full API coverage** — Task creation, claiming, results, webhooks, batch operations, and admin operations
- **Batch operations** — Create, claim, and submit results for multiple tasks in a single request
- **Type hints throughout** — Complete type annotations with PEP 561 marker
- **Async and sync variants** — `CodeQClient` (async/await) and `SyncCodeQClient` (blocking)
- **Automatic retry** — Exponential backoff on transient failures (via tenacity)
- **Connection pooling** — Powered by httpx
- **Comprehensive docstrings** — Detailed documentation for every class and method

## Installation

```bash
pip install codeq-client
```

## Quick Start

### Producer — Create tasks

```python
import asyncio
from codeq import CodeQClient, CreateTaskOptions

async def main():
    async with CodeQClient(
        base_url="http://localhost:8080",
        producer_token="your-producer-jwt",
    ) as client:
        task = await client.create_task(
            CreateTaskOptions(
                command="GENERATE_MASTER",
                payload={"jobId": "j-123"},
                priority=5,
            )
        )
        print("Created task:", task.id)

asyncio.run(main())
```

### Worker — Claim and process tasks

```python
import asyncio
from codeq import CodeQClient, ClaimTaskOptions, SubmitResultOptions

async def main():
    async with CodeQClient(
        base_url="http://localhost:8080",
        worker_token="your-worker-jwt",
    ) as client:
        # Long-poll for a task
        task = await client.claim_task(
            ClaimTaskOptions(
                commands=["GENERATE_MASTER"],
                lease_seconds=120,
                wait_seconds=10,
            )
        )

        if task:
            try:
                output = await do_work(task.payload)
                await client.submit_result(
                    task.id,
                    SubmitResultOptions(
                        status="COMPLETED",
                        result=output,
                    ),
                )
            except Exception as e:
                await client.nack(task.id, 30, str(e))

asyncio.run(main())
```

### Synchronous usage

```python
from codeq import SyncCodeQClient, CreateTaskOptions

with SyncCodeQClient(
    base_url="http://localhost:8080",
    producer_token="your-producer-jwt",
) as client:
    task = client.create_task(
        CreateTaskOptions(
            command="GENERATE_MASTER",
            payload={"jobId": "j-123"},
            priority=5,
        )
    )
    print("Created task:", task.id)
```

### Wait for result

```python
from codeq import WaitForResultOptions

task = await client.create_task(
    CreateTaskOptions(
        command="GENERATE_MASTER",
        payload={"jobId": "j-123"},
    )
)

result = await client.wait_for_result(
    task.id,
    WaitForResultOptions(timeout=60.0, poll_interval=2.0),
)
print(result.result.status, result.result.result)
```

### Batch operations

```python
import asyncio
from codeq import (
    CodeQClient,
    CreateTaskOptions,
    BatchClaimOptions,
    BatchSubmitItem,
)

async def main():
    async with CodeQClient(
        base_url="http://localhost:8080",
        producer_token="your-producer-jwt",
        worker_token="your-worker-jwt",
    ) as client:
        # Batch create up to 100 tasks
        results = await client.batch_create_tasks([
            CreateTaskOptions(command="RENDER", payload={"frame": i})
            for i in range(10)
        ])
        for r in results:
            if r.task:
                print("Created:", r.task.id)
            else:
                print("Error:", r.error)

        # Batch claim up to 10 tasks
        tasks = await client.batch_claim_tasks(
            BatchClaimOptions(count=5, commands=["RENDER"], lease_seconds=120)
        )
        print(f"Claimed {len(tasks)} tasks")

        # Batch submit results
        submit_results = await client.batch_submit_results([
            BatchSubmitItem(
                task_id=t.id,
                status="COMPLETED",
                result={"frame": t.payload.get("frame"), "ok": True},
            )
            for t in tasks
        ])
        for sr in submit_results:
            if sr.result:
                print(f"Task {sr.task_id}: {sr.result.status}")
            else:
                print(f"Task {sr.task_id} error: {sr.error}")

asyncio.run(main())
```

### Webhook subscriptions

```python
from codeq import CreateSubscriptionOptions

sub = await client.create_subscription(
    CreateSubscriptionOptions(
        callback_url="https://myapp.example.com/webhook",
        event_types=["GENERATE_MASTER"],
        ttl_seconds=3600,
        delivery_mode="group",
        group_id="worker-pool-1",
    )
)
print("Subscription:", sub.subscription_id, "expires:", sub.expires_at)

# Renew before expiration
from codeq import RenewSubscriptionOptions
await client.renew_subscription(
    sub.subscription_id,
    RenewSubscriptionOptions(ttl_seconds=3600),
)
```

### Admin operations

```python
from codeq import CodeQClient, CleanupOptions

admin = CodeQClient(
    base_url="http://localhost:8080",
    admin_token="your-admin-jwt",
)

# List all queues
queues = await admin.list_queues()
for q in queues:
    print(q.command, q.ready, q.in_progress)

# Get stats for a specific command
stats = await admin.get_queue_stats("GENERATE_MASTER")

# Cleanup expired tasks
result = await admin.cleanup_expired(CleanupOptions(limit=500))
print("Deleted:", result.deleted)

await admin.close()
```

## API Reference

### `CodeQClient(base_url, **kwargs)` / `SyncCodeQClient(base_url, **kwargs)`

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `base_url` | `str` | — | CodeQ server URL |
| `producer_token` | `str \| None` | `None` | JWT for producer operations |
| `worker_token` | `str \| None` | `None` | JWT for worker operations |
| `admin_token` | `str \| None` | `None` | JWT for admin operations |
| `timeout` | `float` | `30.0` | Request timeout in seconds |
| `retries` | `int` | `3` | Automatic retry count |

### Producer Methods

| Method | Description |
|--------|-------------|
| `create_task(options)` | Create a new task |
| `batch_create_tasks(tasks)` | Create up to 100 tasks in one request |

### Worker Methods

| Method | Description |
|--------|-------------|
| `claim_task(options)` | Claim a task (returns `None` if none available) |
| `batch_claim_tasks(options)` | Claim up to 10 tasks in one request |
| `submit_result(task_id, options)` | Submit a completed/failed result |
| `batch_submit_results(items)` | Submit results for up to 100 tasks in one request |
| `heartbeat(task_id, extend_seconds=300)` | Extend task lease |
| `abandon(task_id)` | Return task to queue |
| `nack(task_id, delay_seconds, reason)` | Negative ack with retry delay |
| `create_subscription(options)` | Register webhook subscription |
| `renew_subscription(subscription_id, options=None)` | Renew subscription TTL |

### Query Methods

| Method | Description |
|--------|-------------|
| `get_task(task_id)` | Get task by ID |
| `get_result(task_id)` | Get task result |
| `wait_for_result(task_id, options=None)` | Poll until result is available |

### Admin Methods

| Method | Description |
|--------|-------------|
| `list_queues()` | List all queue statistics |
| `get_queue_stats(command)` | Get stats for a specific queue |
| `cleanup_expired(options=None)` | Remove expired tasks |

## Types

All types are exported from the `codeq` package:

```python
from codeq import (
    CodeQClient,
    SyncCodeQClient,
    CodeQError,
    CodeQAPIError,
    CodeQAuthError,
    CodeQTimeoutError,
    Task,
    TaskStatus,
    CreateTaskOptions,
    ClaimTaskOptions,
    ArtifactIn,
    ArtifactOut,
    SubmitResultOptions,
    ResultRecord,
    TaskResult,
    NackResponse,
    QueueStats,
    CreateSubscriptionOptions,
    SubscriptionResponse,
    RenewSubscriptionOptions,
    WaitForResultOptions,
    CleanupOptions,
    CleanupResult,
    BatchCreateResult,
    BatchClaimOptions,
    BatchSubmitItem,
    BatchSubmitResult,
)
```

## Development

```bash
# Install in development mode
cd sdks/python
pip install -e ".[dev]"

# Run tests with coverage
pytest --cov

# Type check
mypy src/codeq
```

## License

MIT
