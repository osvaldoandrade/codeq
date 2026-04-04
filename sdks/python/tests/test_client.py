"""Comprehensive tests for the async CodeQ client."""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch

import httpx
import pytest
import respx

from codeq import (
    ClaimTaskOptions,
    CleanupOptions,
    CodeQAPIError,
    CodeQAuthError,
    CodeQClient,
    CodeQTimeoutError,
    CreateSubscriptionOptions,
    CreateTaskOptions,
    NackResponse,
    RenewSubscriptionOptions,
    SubmitResultOptions,
    WaitForResultOptions,
)

BASE_URL = "http://localhost:8080"

SAMPLE_TASK = {
    "id": "task-123",
    "command": "GENERATE_MASTER",
    "payload": {"jobId": "j-123"},
    "status": "PENDING",
    "priority": 5,
    "createdAt": "2025-01-01T00:00:00Z",
    "updatedAt": "2025-01-01T00:00:00Z",
}

SAMPLE_RESULT = {
    "taskId": "task-123",
    "status": "COMPLETED",
    "result": {"output": "done"},
    "completedAt": "2025-01-01T00:01:00Z",
}


# ──────────────────────────────────────────────
# Constructor Tests
# ──────────────────────────────────────────────


class TestConstructor:
    async def test_strips_trailing_slash(self) -> None:
        client = CodeQClient(base_url="http://localhost:8080/", producer_token="t")
        assert client._base_url == "http://localhost:8080"
        await client.close()

    async def test_stores_tokens(self) -> None:
        client = CodeQClient(
            base_url=BASE_URL,
            producer_token="pt",
            worker_token="wt",
            admin_token="at",
        )
        assert client._producer_token == "pt"
        assert client._worker_token == "wt"
        assert client._admin_token == "at"
        await client.close()

    async def test_custom_timeout(self) -> None:
        client = CodeQClient(base_url=BASE_URL, timeout=60.0)
        assert client._http.timeout.connect == 60.0
        await client.close()

    async def test_context_manager(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            assert client._http is not None


# ──────────────────────────────────────────────
# create_task Tests
# ──────────────────────────────────────────────


class TestCreateTask:
    @respx.mock
    async def test_creates_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks").mock(
            return_value=httpx.Response(202, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            task = await client.create_task(
                CreateTaskOptions(
                    command="GENERATE_MASTER",
                    payload={"jobId": "j-123"},
                    priority=5,
                )
            )
        assert task.id == "task-123"
        assert task.command == "GENERATE_MASTER"
        assert task.payload == {"jobId": "j-123"}
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer pt"

    @respx.mock
    async def test_passes_run_at_and_idempotency_key(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks").mock(
            return_value=httpx.Response(202, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            await client.create_task(
                CreateTaskOptions(
                    command="CMD",
                    payload={},
                    run_at="2025-06-01T00:00:00Z",
                    idempotency_key="key-1",
                )
            )
        import json

        body = json.loads(route.calls.last.request.content)
        assert body["runAt"] == "2025-06-01T00:00:00Z"
        assert body["idempotencyKey"] == "key-1"

    async def test_throws_without_producer_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Producer token"):
                await client.create_task(
                    CreateTaskOptions(command="CMD", payload={})
                )

    @respx.mock
    async def test_wraps_api_errors(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks").mock(
            return_value=httpx.Response(400, json={"error": "bad request"})
        )
        async with CodeQClient(
            base_url=BASE_URL, producer_token="pt", retries=0
        ) as client:
            with pytest.raises(CodeQAPIError) as exc_info:
                await client.create_task(
                    CreateTaskOptions(command="CMD", payload={})
                )
            assert exc_info.value.status_code == 400


# ──────────────────────────────────────────────
# claim_task Tests
# ──────────────────────────────────────────────


class TestClaimTask:
    @respx.mock
    async def test_claims_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = await client.claim_task(
                ClaimTaskOptions(commands=["GENERATE_MASTER"])
            )
        assert task is not None
        assert task.id == "task-123"
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer wt"

    @respx.mock
    async def test_applies_defaults(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.claim_task(ClaimTaskOptions(commands=["CMD"]))
        body = json.loads(route.calls.last.request.content)
        assert body["leaseSeconds"] == 300
        assert body["waitSeconds"] == 0

    @respx.mock
    async def test_returns_none_on_204(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/claim").mock(
            return_value=httpx.Response(204)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = await client.claim_task(ClaimTaskOptions(commands=["CMD"]))
        assert task is None

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.claim_task(ClaimTaskOptions(commands=["CMD"]))


# ──────────────────────────────────────────────
# submit_result Tests
# ──────────────────────────────────────────────


class TestSubmitResult:
    @respx.mock
    async def test_submits_result(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(200, json=SAMPLE_RESULT)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            record = await client.submit_result(
                "task-123",
                SubmitResultOptions(status="COMPLETED", result={"output": "done"}),
            )
        assert record.task_id == "task-123"
        assert record.status == "COMPLETED"
        assert route.called

    @respx.mock
    async def test_supports_artifacts(self) -> None:
        import json
        from codeq import ArtifactIn

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(200, json=SAMPLE_RESULT)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.submit_result(
                "task-123",
                SubmitResultOptions(
                    status="COMPLETED",
                    artifacts=[ArtifactIn(name="report.pdf", url="https://cdn/r.pdf")],
                ),
            )
        body = json.loads(route.calls.last.request.content)
        assert body["artifacts"][0]["name"] == "report.pdf"
        assert body["artifacts"][0]["url"] == "https://cdn/r.pdf"

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.submit_result(
                    "t-1", SubmitResultOptions(status="COMPLETED")
                )


# ──────────────────────────────────────────────
# heartbeat Tests
# ──────────────────────────────────────────────


class TestHeartbeat:
    @respx.mock
    async def test_default_extend_seconds(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/heartbeat").mock(
            return_value=httpx.Response(200, json={})
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.heartbeat("task-123")
        body = json.loads(route.calls.last.request.content)
        assert body["extendSeconds"] == 300

    @respx.mock
    async def test_custom_extend_seconds(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/heartbeat").mock(
            return_value=httpx.Response(200, json={})
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.heartbeat("task-123", 120)
        body = json.loads(route.calls.last.request.content)
        assert body["extendSeconds"] == 120

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.heartbeat("t-1")


# ──────────────────────────────────────────────
# abandon Tests
# ──────────────────────────────────────────────


class TestAbandon:
    @respx.mock
    async def test_abandons_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/abandon").mock(
            return_value=httpx.Response(200, json={})
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.abandon("task-123")
        assert route.called

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.abandon("t-1")


# ──────────────────────────────────────────────
# nack Tests
# ──────────────────────────────────────────────


class TestNack:
    @respx.mock
    async def test_nacks_task(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/nack").mock(
            return_value=httpx.Response(
                200, json={"status": "requeued", "delaySeconds": 30}
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            resp = await client.nack("task-123", 30, "temporary failure")
        assert resp.status == "requeued"
        assert resp.delay_seconds == 30
        body = json.loads(route.calls.last.request.content)
        assert body["delaySeconds"] == 30
        assert body["reason"] == "temporary failure"

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.nack("t-1", 30, "reason")


# ──────────────────────────────────────────────
# create_subscription Tests
# ──────────────────────────────────────────────


class TestCreateSubscription:
    @respx.mock
    async def test_creates_subscription(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/workers/subscriptions").mock(
            return_value=httpx.Response(
                200,
                json={
                    "subscriptionId": "sub-1",
                    "expiresAt": "2025-01-01T01:00:00Z",
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            sub = await client.create_subscription(
                CreateSubscriptionOptions(
                    callback_url="https://myapp.com/webhook",
                    event_types=["GENERATE_MASTER"],
                    delivery_mode="group",
                    group_id="pool-1",
                )
            )
        assert sub.subscription_id == "sub-1"
        assert sub.expires_at == "2025-01-01T01:00:00Z"
        assert route.called

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.create_subscription(
                    CreateSubscriptionOptions(callback_url="https://x.com/wh")
                )


# ──────────────────────────────────────────────
# renew_subscription Tests
# ──────────────────────────────────────────────


class TestRenewSubscription:
    @respx.mock
    async def test_renews_subscription(self) -> None:
        route = respx.post(
            f"{BASE_URL}/v1/codeq/workers/subscriptions/sub-1/heartbeat"
        ).mock(
            return_value=httpx.Response(
                200,
                json={
                    "subscriptionId": "sub-1",
                    "expiresAt": "2025-01-01T02:00:00Z",
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            sub = await client.renew_subscription(
                "sub-1", RenewSubscriptionOptions(ttl_seconds=7200)
            )
        assert sub.expires_at == "2025-01-01T02:00:00Z"
        assert route.called

    @respx.mock
    async def test_sends_empty_body_without_options(self) -> None:
        import json

        route = respx.post(
            f"{BASE_URL}/v1/codeq/workers/subscriptions/sub-1/heartbeat"
        ).mock(
            return_value=httpx.Response(
                200,
                json={
                    "subscriptionId": "sub-1",
                    "expiresAt": "2025-01-01T02:00:00Z",
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            await client.renew_subscription("sub-1")
        body = json.loads(route.calls.last.request.content)
        assert body == {}

    @respx.mock
    async def test_falls_back_to_producer_token(self) -> None:
        route = respx.post(
            f"{BASE_URL}/v1/codeq/workers/subscriptions/sub-1/heartbeat"
        ).mock(
            return_value=httpx.Response(
                200,
                json={
                    "subscriptionId": "sub-1",
                    "expiresAt": "2025-01-01T02:00:00Z",
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            await client.renew_subscription("sub-1")
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer pt"

    async def test_throws_without_any_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Token is required"):
                await client.renew_subscription("sub-1")


# ──────────────────────────────────────────────
# get_task Tests
# ──────────────────────────────────────────────


class TestGetTask:
    @respx.mock
    async def test_gets_task(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = await client.get_task("task-123")
        assert task.id == "task-123"
        assert route.called

    @respx.mock
    async def test_falls_back_to_producer_token(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            await client.get_task("task-123")
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer pt"

    async def test_throws_without_any_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Token is required"):
                await client.get_task("t-1")


# ──────────────────────────────────────────────
# get_result Tests
# ──────────────────────────────────────────────


class TestGetResult:
    @respx.mock
    async def test_gets_result(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            tr = await client.get_result("task-123")
        assert tr.task.id == "task-123"
        assert tr.result.task_id == "task-123"
        assert tr.result.status == "COMPLETED"
        assert route.called

    async def test_throws_without_any_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Token is required"):
                await client.get_result("t-1")


# ──────────────────────────────────────────────
# wait_for_result Tests
# ──────────────────────────────────────────────


class TestWaitForResult:
    @respx.mock
    async def test_returns_immediately_when_available(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            tr = await client.wait_for_result("task-123")
        assert tr.result.status == "COMPLETED"

    @respx.mock
    async def test_polls_until_available(self) -> None:
        call_count = 0

        def side_effect(request: httpx.Request) -> httpx.Response:
            nonlocal call_count
            call_count += 1
            if call_count < 3:
                return httpx.Response(404, json={"error": "not found"})
            return httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )

        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            side_effect=side_effect
        )
        async with CodeQClient(
            base_url=BASE_URL, worker_token="wt", retries=0
        ) as client:
            tr = await client.wait_for_result(
                "task-123",
                WaitForResultOptions(timeout=10.0, poll_interval=0.01),
            )
        assert tr.result.status == "COMPLETED"
        assert call_count == 3

    @respx.mock
    async def test_throws_timeout(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(404, json={"error": "not found"})
        )
        async with CodeQClient(
            base_url=BASE_URL, worker_token="wt", retries=0
        ) as client:
            with pytest.raises(CodeQTimeoutError, match="Timed out"):
                await client.wait_for_result(
                    "task-123",
                    WaitForResultOptions(timeout=0.1, poll_interval=0.01),
                )

    @respx.mock
    async def test_uses_default_options(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            tr = await client.wait_for_result("task-123")
        assert tr.result.status == "COMPLETED"


# ──────────────────────────────────────────────
# Admin Operations Tests
# ──────────────────────────────────────────────


class TestListQueues:
    @respx.mock
    async def test_lists_queues(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/admin/queues").mock(
            return_value=httpx.Response(
                200,
                json=[
                    {
                        "command": "CMD_A",
                        "ready": 10,
                        "delayed": 2,
                        "inProgress": 3,
                        "dlq": 0,
                    }
                ],
            )
        )
        async with CodeQClient(base_url=BASE_URL, admin_token="at") as client:
            queues = await client.list_queues()
        assert len(queues) == 1
        assert queues[0].command == "CMD_A"
        assert queues[0].ready == 10
        assert queues[0].in_progress == 3
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer at"

    async def test_throws_without_admin_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Admin token"):
                await client.list_queues()


class TestGetQueueStats:
    @respx.mock
    async def test_gets_queue_stats(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/admin/queues/GENERATE_MASTER").mock(
            return_value=httpx.Response(
                200,
                json={
                    "command": "GENERATE_MASTER",
                    "ready": 5,
                    "delayed": 1,
                    "inProgress": 2,
                    "dlq": 0,
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, admin_token="at") as client:
            stats = await client.get_queue_stats("GENERATE_MASTER")
        assert stats.command == "GENERATE_MASTER"
        assert stats.ready == 5
        assert route.called

    async def test_throws_without_admin_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Admin token"):
                await client.get_queue_stats("CMD")


class TestCleanupExpired:
    @respx.mock
    async def test_cleans_up(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/admin/tasks/cleanup").mock(
            return_value=httpx.Response(
                200,
                json={
                    "deleted": 42,
                    "before": "2025-01-01T00:00:00Z",
                    "limit": 1000,
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, admin_token="at") as client:
            result = await client.cleanup_expired()
        assert result.deleted == 42
        assert result.limit == 1000
        assert route.called

    @respx.mock
    async def test_passes_options(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/admin/tasks/cleanup").mock(
            return_value=httpx.Response(
                200,
                json={
                    "deleted": 5,
                    "before": "2025-01-01T00:00:00Z",
                    "limit": 500,
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, admin_token="at") as client:
            await client.cleanup_expired(CleanupOptions(limit=500))
        body = json.loads(route.calls.last.request.content)
        assert body["limit"] == 500

    async def test_throws_without_admin_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Admin token"):
                await client.cleanup_expired()


# ──────────────────────────────────────────────
# URL Encoding Tests
# ──────────────────────────────────────────────


class TestURLEncoding:
    @respx.mock
    async def test_encodes_special_characters(self) -> None:
        route = respx.get(
            f"{BASE_URL}/v1/codeq/tasks/task%2Fspecial"
        ).mock(return_value=httpx.Response(200, json=SAMPLE_TASK))
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = await client.get_task("task/special")
        assert task.id == "task-123"
        assert route.called


# ──────────────────────────────────────────────
# batch_create_tasks Tests
# ──────────────────────────────────────────────


class TestBatchCreateTasks:
    @respx.mock
    async def test_creates_tasks_in_batch(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"task": SAMPLE_TASK},
                        {"task": {**SAMPLE_TASK, "id": "task-456", "command": "CMD2"}},
                    ]
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            results = await client.batch_create_tasks(
                [
                    CreateTaskOptions(command="GENERATE_MASTER", payload={"jobId": "j-1"}),
                    CreateTaskOptions(command="CMD2", payload={"jobId": "j-2"}),
                ]
            )
        assert len(results) == 2
        assert results[0].task is not None
        assert results[0].task.id == "task-123"
        assert results[0].error is None
        assert results[1].task is not None
        assert results[1].task.id == "task-456"
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer pt"

    @respx.mock
    async def test_handles_partial_failure(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/batch").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"task": SAMPLE_TASK},
                        {"error": "duplicate idempotency key"},
                    ]
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            results = await client.batch_create_tasks(
                [
                    CreateTaskOptions(command="CMD", payload={}),
                    CreateTaskOptions(command="CMD", payload={}, idempotency_key="dup"),
                ]
            )
        assert len(results) == 2
        assert results[0].task is not None
        assert results[0].error is None
        assert results[1].task is None
        assert results[1].error == "duplicate idempotency key"

    @respx.mock
    async def test_sends_correct_body(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch").mock(
            return_value=httpx.Response(200, json={"results": [{"task": SAMPLE_TASK}]})
        )
        async with CodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            await client.batch_create_tasks(
                [
                    CreateTaskOptions(
                        command="CMD",
                        payload={"k": "v"},
                        priority=3,
                        max_attempts=8,
                    )
                ]
            )
        body = json.loads(route.calls.last.request.content)
        assert "tasks" in body
        assert len(body["tasks"]) == 1
        assert body["tasks"][0]["command"] == "CMD"
        assert body["tasks"][0]["priority"] == 3
        assert body["tasks"][0]["maxAttempts"] == 8

    async def test_throws_without_producer_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Producer token"):
                await client.batch_create_tasks(
                    [CreateTaskOptions(command="CMD", payload={})]
                )


# ──────────────────────────────────────────────
# batch_claim_tasks Tests
# ──────────────────────────────────────────────


class TestBatchClaimTasks:
    @respx.mock
    async def test_claims_multiple_tasks(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim/batch").mock(
            return_value=httpx.Response(
                200,
                json={
                    "tasks": [
                        SAMPLE_TASK,
                        {**SAMPLE_TASK, "id": "task-456"},
                    ]
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchClaimOptions

            tasks = await client.batch_claim_tasks(
                BatchClaimOptions(count=5, commands=["GENERATE_MASTER"])
            )
        assert len(tasks) == 2
        assert tasks[0].id == "task-123"
        assert tasks[1].id == "task-456"
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer wt"

    @respx.mock
    async def test_returns_empty_on_204(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/claim/batch").mock(
            return_value=httpx.Response(204)
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchClaimOptions

            tasks = await client.batch_claim_tasks(BatchClaimOptions(count=5))
        assert tasks == []

    @respx.mock
    async def test_sends_correct_body(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim/batch").mock(
            return_value=httpx.Response(200, json={"tasks": [SAMPLE_TASK]})
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchClaimOptions

            await client.batch_claim_tasks(
                BatchClaimOptions(count=3, commands=["CMD1", "CMD2"], lease_seconds=120)
            )
        body = json.loads(route.calls.last.request.content)
        assert body["count"] == 3
        assert body["commands"] == ["CMD1", "CMD2"]
        assert body["leaseSeconds"] == 120

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            from codeq import BatchClaimOptions

            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.batch_claim_tasks(BatchClaimOptions(count=5))


# ──────────────────────────────────────────────
# batch_submit_results Tests
# ──────────────────────────────────────────────


class TestBatchSubmitResults:
    @respx.mock
    async def test_submits_multiple_results(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch/results").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"taskId": "task-123", "result": SAMPLE_RESULT},
                        {"taskId": "task-456", "result": {**SAMPLE_RESULT, "taskId": "task-456"}},
                    ]
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchSubmitItem

            results = await client.batch_submit_results(
                [
                    BatchSubmitItem(
                        task_id="task-123",
                        status="COMPLETED",
                        result={"output": "done"},
                    ),
                    BatchSubmitItem(
                        task_id="task-456",
                        status="COMPLETED",
                        result={"output": "done"},
                    ),
                ]
            )
        assert len(results) == 2
        assert results[0].task_id == "task-123"
        assert results[0].result is not None
        assert results[0].result.status == "COMPLETED"
        assert results[0].error is None
        assert results[1].task_id == "task-456"
        assert route.called
        req = route.calls.last.request
        assert req.headers["Authorization"] == "Bearer wt"

    @respx.mock
    async def test_handles_partial_failure(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/batch/results").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"taskId": "task-123", "result": SAMPLE_RESULT},
                        {"taskId": "task-456", "error": "task not found"},
                    ]
                },
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchSubmitItem

            results = await client.batch_submit_results(
                [
                    BatchSubmitItem(task_id="task-123", status="COMPLETED", result={}),
                    BatchSubmitItem(task_id="task-456", status="FAILED", error="oops"),
                ]
            )
        assert len(results) == 2
        assert results[0].result is not None
        assert results[0].error is None
        assert results[1].result is None
        assert results[1].error == "task not found"

    @respx.mock
    async def test_sends_correct_body(self) -> None:
        import json

        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch/results").mock(
            return_value=httpx.Response(
                200, json={"results": [{"taskId": "t-1", "result": SAMPLE_RESULT}]}
            )
        )
        async with CodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchSubmitItem

            await client.batch_submit_results(
                [
                    BatchSubmitItem(
                        task_id="t-1",
                        status="COMPLETED",
                        result={"ok": True},
                    )
                ]
            )
        body = json.loads(route.calls.last.request.content)
        assert "results" in body
        assert len(body["results"]) == 1
        assert body["results"][0]["taskId"] == "t-1"
        assert body["results"][0]["status"] == "COMPLETED"

    async def test_throws_without_worker_token(self) -> None:
        async with CodeQClient(base_url=BASE_URL) as client:
            from codeq import BatchSubmitItem

            with pytest.raises(CodeQAuthError, match="Worker token"):
                await client.batch_submit_results(
                    [BatchSubmitItem(task_id="t-1", status="COMPLETED")]
                )
