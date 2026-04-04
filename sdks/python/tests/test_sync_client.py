"""Tests for the synchronous CodeQ client."""

from __future__ import annotations

import json
import time

import httpx
import pytest
import respx

from codeq import (
    ClaimTaskOptions,
    CleanupOptions,
    CodeQAPIError,
    CodeQAuthError,
    CodeQTimeoutError,
    CreateSubscriptionOptions,
    CreateTaskOptions,
    NackResponse,
    RenewSubscriptionOptions,
    SubmitResultOptions,
    SyncCodeQClient,
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


class TestSyncConstructor:
    def test_strips_trailing_slash(self) -> None:
        client = SyncCodeQClient(base_url="http://localhost:8080/", producer_token="t")
        assert client._base_url == "http://localhost:8080"
        client.close()

    def test_context_manager(self) -> None:
        with SyncCodeQClient(base_url=BASE_URL) as client:
            assert client._http is not None


class TestSyncCreateTask:
    @respx.mock
    def test_creates_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks").mock(
            return_value=httpx.Response(202, json=SAMPLE_TASK)
        )
        with SyncCodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            task = client.create_task(
                CreateTaskOptions(
                    command="GENERATE_MASTER",
                    payload={"jobId": "j-123"},
                    priority=5,
                )
            )
        assert task.id == "task-123"
        assert task.command == "GENERATE_MASTER"
        assert route.called

    def test_throws_without_producer_token(self) -> None:
        with SyncCodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Producer token"):
                client.create_task(CreateTaskOptions(command="CMD", payload={}))


class TestSyncClaimTask:
    @respx.mock
    def test_claims_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = client.claim_task(ClaimTaskOptions(commands=["CMD"]))
        assert task is not None
        assert task.id == "task-123"

    @respx.mock
    def test_returns_none_on_204(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/claim").mock(
            return_value=httpx.Response(204)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = client.claim_task(ClaimTaskOptions(commands=["CMD"]))
        assert task is None


class TestSyncSubmitResult:
    @respx.mock
    def test_submits_result(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(200, json=SAMPLE_RESULT)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            record = client.submit_result(
                "task-123",
                SubmitResultOptions(status="COMPLETED", result={"output": "done"}),
            )
        assert record.task_id == "task-123"
        assert record.status == "COMPLETED"


class TestSyncHeartbeat:
    @respx.mock
    def test_heartbeat(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/heartbeat").mock(
            return_value=httpx.Response(200, json={})
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            client.heartbeat("task-123", 120)
        body = json.loads(route.calls.last.request.content)
        assert body["extendSeconds"] == 120


class TestSyncAbandon:
    @respx.mock
    def test_abandons_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/abandon").mock(
            return_value=httpx.Response(200, json={})
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            client.abandon("task-123")
        assert route.called


class TestSyncNack:
    @respx.mock
    def test_nacks_task(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/task-123/nack").mock(
            return_value=httpx.Response(
                200, json={"status": "requeued", "delaySeconds": 30}
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            resp = client.nack("task-123", 30, "temporary failure")
        assert resp.status == "requeued"
        assert resp.delay_seconds == 30


class TestSyncSubscription:
    @respx.mock
    def test_creates_subscription(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/workers/subscriptions").mock(
            return_value=httpx.Response(
                200,
                json={
                    "subscriptionId": "sub-1",
                    "expiresAt": "2025-01-01T01:00:00Z",
                },
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            sub = client.create_subscription(
                CreateSubscriptionOptions(callback_url="https://x.com/wh")
            )
        assert sub.subscription_id == "sub-1"

    @respx.mock
    def test_renews_subscription(self) -> None:
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
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            sub = client.renew_subscription("sub-1")
        assert sub.expires_at == "2025-01-01T02:00:00Z"


class TestSyncGetTask:
    @respx.mock
    def test_gets_task(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = client.get_task("task-123")
        assert task.id == "task-123"


class TestSyncGetResult:
    @respx.mock
    def test_gets_result(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            tr = client.get_result("task-123")
        assert tr.task.id == "task-123"
        assert tr.result.status == "COMPLETED"


class TestSyncWaitForResult:
    @respx.mock
    def test_returns_immediately_when_available(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(
                200, json={"task": SAMPLE_TASK, "result": SAMPLE_RESULT}
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            tr = client.wait_for_result("task-123")
        assert tr.result.status == "COMPLETED"

    @respx.mock
    def test_throws_timeout(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/tasks/task-123/result").mock(
            return_value=httpx.Response(404, json={"error": "not found"})
        )
        with SyncCodeQClient(
            base_url=BASE_URL, worker_token="wt", retries=0
        ) as client:
            with pytest.raises(CodeQTimeoutError, match="Timed out"):
                client.wait_for_result(
                    "task-123",
                    WaitForResultOptions(timeout=0.1, poll_interval=0.01),
                )


class TestSyncAdmin:
    @respx.mock
    def test_lists_queues(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/admin/queues").mock(
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
        with SyncCodeQClient(base_url=BASE_URL, admin_token="at") as client:
            queues = client.list_queues()
        assert len(queues) == 1
        assert queues[0].command == "CMD_A"

    @respx.mock
    def test_gets_queue_stats(self) -> None:
        respx.get(f"{BASE_URL}/v1/codeq/admin/queues/CMD").mock(
            return_value=httpx.Response(
                200,
                json={
                    "command": "CMD",
                    "ready": 5,
                    "delayed": 0,
                    "inProgress": 1,
                    "dlq": 0,
                },
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, admin_token="at") as client:
            stats = client.get_queue_stats("CMD")
        assert stats.ready == 5

    @respx.mock
    def test_cleanup_expired(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/admin/tasks/cleanup").mock(
            return_value=httpx.Response(
                200,
                json={
                    "deleted": 10,
                    "before": "2025-01-01T00:00:00Z",
                    "limit": 1000,
                },
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, admin_token="at") as client:
            result = client.cleanup_expired()
        assert result.deleted == 10


class TestSyncURLEncoding:
    @respx.mock
    def test_encodes_special_characters(self) -> None:
        route = respx.get(f"{BASE_URL}/v1/codeq/tasks/task%2Fspecial").mock(
            return_value=httpx.Response(200, json=SAMPLE_TASK)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            task = client.get_task("task/special")
        assert task.id == "task-123"
        assert route.called


# ──────────────────────────────────────────────
# Batch Operations (Sync)
# ──────────────────────────────────────────────


class TestSyncBatchCreateTasks:
    @respx.mock
    def test_creates_tasks_in_batch(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"task": SAMPLE_TASK},
                        {"task": {**SAMPLE_TASK, "id": "task-456"}},
                    ]
                },
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, producer_token="pt") as client:
            results = client.batch_create_tasks(
                [
                    CreateTaskOptions(command="CMD", payload={}),
                    CreateTaskOptions(command="CMD2", payload={}),
                ]
            )
        assert len(results) == 2
        assert results[0].task is not None
        assert results[0].task.id == "task-123"
        assert results[1].task is not None
        assert results[1].task.id == "task-456"
        assert route.called

    def test_throws_without_producer_token(self) -> None:
        with SyncCodeQClient(base_url=BASE_URL) as client:
            with pytest.raises(CodeQAuthError, match="Producer token"):
                client.batch_create_tasks(
                    [CreateTaskOptions(command="CMD", payload={})]
                )


class TestSyncBatchClaimTasks:
    @respx.mock
    def test_claims_multiple_tasks(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/claim/batch").mock(
            return_value=httpx.Response(
                200,
                json={"tasks": [SAMPLE_TASK, {**SAMPLE_TASK, "id": "task-456"}]},
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchClaimOptions

            tasks = client.batch_claim_tasks(
                BatchClaimOptions(count=5, commands=["CMD"])
            )
        assert len(tasks) == 2
        assert tasks[0].id == "task-123"
        assert route.called

    @respx.mock
    def test_returns_empty_on_204(self) -> None:
        respx.post(f"{BASE_URL}/v1/codeq/tasks/claim/batch").mock(
            return_value=httpx.Response(204)
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchClaimOptions

            tasks = client.batch_claim_tasks(BatchClaimOptions(count=5))
        assert tasks == []


class TestSyncBatchSubmitResults:
    @respx.mock
    def test_submits_multiple_results(self) -> None:
        route = respx.post(f"{BASE_URL}/v1/codeq/tasks/batch/results").mock(
            return_value=httpx.Response(
                200,
                json={
                    "results": [
                        {"taskId": "task-123", "result": SAMPLE_RESULT},
                    ]
                },
            )
        )
        with SyncCodeQClient(base_url=BASE_URL, worker_token="wt") as client:
            from codeq import BatchSubmitItem

            results = client.batch_submit_results(
                [
                    BatchSubmitItem(
                        task_id="task-123", status="COMPLETED", result={"ok": True}
                    ),
                ]
            )
        assert len(results) == 1
        assert results[0].task_id == "task-123"
        assert results[0].result is not None
        assert route.called

    def test_throws_without_worker_token(self) -> None:
        with SyncCodeQClient(base_url=BASE_URL) as client:
            from codeq import BatchSubmitItem

            with pytest.raises(CodeQAuthError, match="Worker token"):
                client.batch_submit_results(
                    [BatchSubmitItem(task_id="t-1", status="COMPLETED")]
                )
