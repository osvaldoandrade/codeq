# Python Integration Guide

Complete guide for integrating CodeQ with Python microservices using FastAPI, Django, and Flask.

## Table of Contents

- [Overview](#overview)
- [SDK Installation](#sdk-installation)
- [FastAPI Integration](#fastapi-integration)
- [Django Integration](#django-integration)
- [Flask Integration](#flask-integration)
- [Best Practices](#best-practices)
  - [Connection Management](#connection-management)
  - [Error Handling](#error-handling)
  - [Heartbeat Pattern](#heartbeat-pattern)
  - [Batch Operations](#batch-operations)
- [Troubleshooting](#troubleshooting)

## Overview

The CodeQ Python SDK provides both async and sync APIs with full type hints for:
- **Producing tasks**: Create tasks with priority, webhooks, and delays
- **Consuming tasks**: Claim, process, and complete tasks as a worker
- **Task lifecycle**: Heartbeat, abandon, and NACK operations

### Architecture

```
┌─────────────────┐         ┌─────────────┐         ┌──────────────┐
│  FastAPI/Django  │────────▶│   CodeQ     │◀────────│   Worker     │
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

### pip

```bash
pip install codeq-client
```

### poetry

```bash
poetry add codeq-client
```

### With development dependencies

```bash
pip install "codeq-client[dev]"
```

## FastAPI Integration

### Producer Service

```python
from contextlib import asynccontextmanager

from fastapi import FastAPI
from codeq import CodeQClient, CreateTaskOptions

client: CodeQClient


@asynccontextmanager
async def lifespan(app: FastAPI):
    global client
    client = CodeQClient(
        base_url="http://localhost:8080",
        producer_token="your-producer-jwt",
    )
    yield
    await client.close()


app = FastAPI(lifespan=lifespan)


@app.post("/jobs")
async def create_job(job_id: str, priority: int = 0):
    task = await client.create_task(
        CreateTaskOptions(
            command="PROCESS_JOB",
            payload={"jobId": job_id},
            priority=priority,
        )
    )
    return {"taskId": task.id, "status": task.status}
```

### Worker Service

```python
import asyncio
from codeq import (
    CodeQClient,
    ClaimTaskOptions,
    SubmitResultOptions,
)


async def worker_loop():
    async with CodeQClient(
        base_url="http://localhost:8080",
        worker_token="your-worker-jwt",
    ) as client:
        while True:
            task = await client.claim_task(
                ClaimTaskOptions(
                    commands=["PROCESS_JOB"],
                    lease_seconds=120,
                    wait_seconds=10,
                )
            )

            if not task:
                continue

            try:
                result = await process(task.payload)
                await client.submit_result(
                    task.id,
                    SubmitResultOptions(
                        status="COMPLETED",
                        result=result,
                    ),
                )
            except Exception as e:
                await client.nack(task.id, 30, str(e))


asyncio.run(worker_loop())
```

## Django Integration

### Configuration

```python
# settings.py
CODEQ_BASE_URL = "http://localhost:8080"
CODEQ_PRODUCER_TOKEN = "your-producer-jwt"
CODEQ_WORKER_TOKEN = "your-worker-jwt"
```

### Synchronous Producer View

```python
# views.py
from django.conf import settings
from django.http import JsonResponse
from codeq import SyncCodeQClient, CreateTaskOptions


def create_job(request):
    with SyncCodeQClient(
        base_url=settings.CODEQ_BASE_URL,
        producer_token=settings.CODEQ_PRODUCER_TOKEN,
    ) as client:
        task = client.create_task(
            CreateTaskOptions(
                command="PROCESS_JOB",
                payload={"jobId": request.POST["job_id"]},
            )
        )
    return JsonResponse({"taskId": task.id, "status": task.status})
```

### Management Command Worker

```python
# management/commands/codeq_worker.py
from django.conf import settings
from django.core.management.base import BaseCommand
from codeq import (
    SyncCodeQClient,
    ClaimTaskOptions,
    SubmitResultOptions,
)


class Command(BaseCommand):
    help = "Run CodeQ worker"

    def handle(self, *args, **options):
        with SyncCodeQClient(
            base_url=settings.CODEQ_BASE_URL,
            worker_token=settings.CODEQ_WORKER_TOKEN,
        ) as client:
            while True:
                task = client.claim_task(
                    ClaimTaskOptions(
                        commands=["PROCESS_JOB"],
                        lease_seconds=120,
                        wait_seconds=10,
                    )
                )
                if task:
                    try:
                        result = self.process(task.payload)
                        client.submit_result(
                            task.id,
                            SubmitResultOptions(
                                status="COMPLETED",
                                result=result,
                            ),
                        )
                    except Exception as e:
                        client.nack(task.id, 30, str(e))
```

## Flask Integration

### Producer Endpoint

```python
from flask import Flask, request, jsonify
from codeq import SyncCodeQClient, CreateTaskOptions

app = Flask(__name__)

client = SyncCodeQClient(
    base_url="http://localhost:8080",
    producer_token="your-producer-jwt",
)


@app.post("/jobs")
def create_job():
    data = request.get_json()
    task = client.create_task(
        CreateTaskOptions(
            command="PROCESS_JOB",
            payload={"jobId": data["jobId"]},
            priority=data.get("priority", 0),
        )
    )
    return jsonify({"taskId": task.id, "status": task.status})


@app.teardown_appcontext
def close_client(exception):
    client.close()
```

## Best Practices

### Connection Management

Always use context managers to ensure proper cleanup:

```python
# Async
async with CodeQClient(base_url="...", producer_token="...") as client:
    await client.create_task(...)

# Sync
with SyncCodeQClient(base_url="...", producer_token="...") as client:
    client.create_task(...)
```

### Error Handling

```python
from codeq import CodeQAPIError, CodeQAuthError, CodeQTimeoutError

try:
    task = await client.create_task(options)
except CodeQAuthError:
    # Missing or invalid token
    logger.error("Authentication failed")
except CodeQAPIError as e:
    # API returned an error (4xx/5xx)
    logger.error(f"API error {e.status_code}: {e.response_body}")
except CodeQTimeoutError:
    # Polling timeout in wait_for_result
    logger.error("Task did not complete in time")
```

### Heartbeat Pattern

For long-running tasks, send periodic heartbeats:

```python
import asyncio

async def process_with_heartbeat(client, task):
    async def heartbeat_loop():
        while True:
            await asyncio.sleep(60)
            await client.heartbeat(task.id, 120)

    heartbeat_task = asyncio.create_task(heartbeat_loop())
    try:
        result = await do_work(task.payload)
        await client.submit_result(
            task.id,
            SubmitResultOptions(status="COMPLETED", result=result),
        )
    finally:
        heartbeat_task.cancel()
```

### Batch Operations

For high-throughput scenarios, use batch operations to improve efficiency:

```python
from codeq import (
    CodeQClient,
    CreateTaskOptions,
    BatchClaimOptions,
    BatchSubmitItem,
)

async def batch_producer_example(client):
    # Batch create up to 100 tasks
    results = await client.batch_create_tasks([
        CreateTaskOptions(
            command="RENDER_FRAME",
            payload={"frame_number": i, "quality": "high"},
            priority=i % 10,
        )
        for i in range(50)
    ])
    
    created_count = sum(1 for r in results if r.task)
    failed_count = sum(1 for r in results if r.error)
    print(f"Created: {created_count}, Failed: {failed_count}")
    
    return [r.task.id for r in results if r.task]


async def batch_worker_example(client):
    # Batch claim up to 10 tasks
    tasks = await client.batch_claim_tasks(
        BatchClaimOptions(
            count=10,
            commands=["RENDER_FRAME"],
            lease_seconds=300,
        )
    )
    print(f"Claimed {len(tasks)} tasks")
    
    # Process tasks
    results_to_submit = []
    for task in tasks:
        try:
            # Simulate processing
            result = {"frame": task.payload["frame_number"], "status": "ok"}
            results_to_submit.append(
                BatchSubmitItem(
                    task_id=task.id,
                    status="COMPLETED",
                    result=result,
                )
            )
        except Exception as e:
            results_to_submit.append(
                BatchSubmitItem(
                    task_id=task.id,
                    status="FAILED",
                    error=str(e),
                )
            )
    
    # Batch submit results
    submit_results = await client.batch_submit_results(results_to_submit)
    
    for sr in submit_results:
        if sr.result:
            print(f"Task {sr.task_id}: {sr.result.status}")
        else:
            print(f"Task {sr.task_id} error: {sr.error}")
```

#### Performance Considerations

- **Batch create**: Up to 100 tasks per request. Useful for bulk imports or scheduled bulk task creation.
- **Batch claim**: Up to 10 tasks per request. Reduces latency in high-throughput worker pools.
- **Batch submit**: Up to 100 results per request. Minimize round-trips when processing multiple tasks.
- **Retry logic**: Batch operations include automatic retry with exponential backoff via tenacity.

#### FastAPI Batch Endpoint Example

```python
from fastapi import FastAPI, BackgroundTasks
from codeq import CodeQClient, CreateTaskOptions

@app.post("/batch-jobs")
async def create_batch_jobs(job_ids: list[str], background_tasks: BackgroundTasks):
    global client
    
    async def batch_create():
        results = await client.batch_create_tasks([
            CreateTaskOptions(
                command="BATCH_PROCESS",
                payload={"jobId": jid},
            )
            for jid in job_ids
        ])
        created = [r.task.id for r in results if r.task]
        return {"created": len(created), "failed": len(results) - len(created)}
    
    background_tasks.add_task(batch_create)
    return {"message": "Batch jobs submitted", "count": len(job_ids)}
```

## Troubleshooting

### Connection Refused

Ensure the CodeQ server is running and accessible:

```python
client = CodeQClient(base_url="http://localhost:8080", ...)
```

### Token Errors

Each operation type requires the appropriate token:
- **Producer operations**: `producer_token`
- **Worker operations**: `worker_token`
- **Admin operations**: `admin_token`
- **Query operations**: Either `worker_token` or `producer_token`

### Timeout Errors

Increase the timeout for long-running operations:

```python
client = CodeQClient(base_url="...", timeout=60.0)

# Or for wait_for_result specifically:
result = await client.wait_for_result(
    task_id,
    WaitForResultOptions(timeout=120.0, poll_interval=2.0),
)
```
