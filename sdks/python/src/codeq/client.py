"""Async CodeQ client implementation."""

from __future__ import annotations

import asyncio
from dataclasses import asdict
from typing import Any
from urllib.parse import quote

import httpx
from tenacity import (
    retry,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)

from .exceptions import CodeQAPIError, CodeQAuthError, CodeQTimeoutError
from .types import (
    ClaimTaskOptions,
    CleanupOptions,
    CleanupResult,
    CreateSubscriptionOptions,
    CreateTaskOptions,
    NackResponse,
    QueueStats,
    RenewSubscriptionOptions,
    ResultRecord,
    SubmitResultOptions,
    SubscriptionResponse,
    Task,
    TaskResult,
    WaitForResultOptions,
)

# Keys that use camelCase in the API JSON but snake_case in Python dataclasses
_SNAKE_TO_CAMEL: dict[str, str] = {
    "created_at": "createdAt",
    "updated_at": "updatedAt",
    "worker_id": "workerId",
    "lease_until": "leaseUntil",
    "max_attempts": "maxAttempts",
    "result_key": "resultKey",
    "tenant_id": "tenantId",
    "task_id": "taskId",
    "completed_at": "completedAt",
    "delay_seconds": "delaySeconds",
    "in_progress": "inProgress",
    "callback_url": "callbackUrl",
    "event_types": "eventTypes",
    "ttl_seconds": "ttlSeconds",
    "delivery_mode": "deliveryMode",
    "group_id": "groupId",
    "min_interval_seconds": "minIntervalSeconds",
    "subscription_id": "subscriptionId",
    "expires_at": "expiresAt",
    "idempotency_key": "idempotencyKey",
    "run_at": "runAt",
    "lease_seconds": "leaseSeconds",
    "wait_seconds": "waitSeconds",
    "content_base64": "contentBase64",
    "content_type": "contentType",
    "poll_interval": "pollInterval",
}

_CAMEL_TO_SNAKE: dict[str, str] = {v: k for k, v in _SNAKE_TO_CAMEL.items()}


def _to_camel(key: str) -> str:
    """Convert a snake_case key to camelCase."""
    return _SNAKE_TO_CAMEL.get(key, key)


def _to_snake(key: str) -> str:
    """Convert a camelCase key to snake_case."""
    return _CAMEL_TO_SNAKE.get(key, key)


def _snake_to_camel_dict(data: dict[str, Any]) -> dict[str, Any]:
    """Convert a dict with snake_case keys to camelCase keys, excluding None values."""
    result: dict[str, Any] = {}
    for key, value in data.items():
        if value is None:
            continue
        camel_key = _to_camel(key)
        if isinstance(value, list):
            result[camel_key] = [
                _snake_to_camel_dict(v) if isinstance(v, dict) else v for v in value
            ]
        elif isinstance(value, dict):
            result[camel_key] = _snake_to_camel_dict(value)
        else:
            result[camel_key] = value
    return result


def _camel_to_snake_dict(data: dict[str, Any]) -> dict[str, Any]:
    """Convert a dict with camelCase keys to snake_case keys."""
    result: dict[str, Any] = {}
    for key, value in data.items():
        snake_key = _to_snake(key)
        if isinstance(value, dict):
            result[snake_key] = _camel_to_snake_dict(value)
        elif isinstance(value, list):
            result[snake_key] = [
                _camel_to_snake_dict(v) if isinstance(v, dict) else v for v in value
            ]
        else:
            result[snake_key] = value
    return result


def _is_retryable(exc: BaseException) -> bool:
    """Check if an exception is retryable (network errors or 5xx)."""
    if isinstance(exc, httpx.TransportError):
        return True
    if isinstance(exc, httpx.HTTPStatusError) and exc.response.status_code >= 500:
        return True
    return False


def _build_task(data: dict[str, Any]) -> Task:
    """Build a Task instance from a camelCase API response dict."""
    snake = _camel_to_snake_dict(data)
    return Task(**{k: v for k, v in snake.items() if k in Task.__dataclass_fields__})


def _build_result_record(data: dict[str, Any]) -> ResultRecord:
    """Build a ResultRecord from a camelCase API response dict."""
    snake = _camel_to_snake_dict(data)
    return ResultRecord(
        **{k: v for k, v in snake.items() if k in ResultRecord.__dataclass_fields__}
    )


def _serialize_options(options: Any) -> dict[str, Any]:
    """Serialize a dataclass options object to a camelCase dict for the API."""
    return _snake_to_camel_dict(asdict(options))


def _encode_id(value: str) -> str:
    """URL-encode a task or subscription ID."""
    return quote(value, safe="")


class CodeQClient:
    """Async client for the CodeQ reactive task scheduling system.

    Provides a complete async/await API for interacting with the CodeQ server.
    Supports producer, worker, and admin operations with automatic retry logic
    and exponential backoff.

    Args:
        base_url: Base URL of the CodeQ server (e.g. ``"http://localhost:8080"``).
        producer_token: JWT token for producer operations (``create_task``).
        worker_token: JWT token for worker operations (claim, heartbeat, result,
            subscribe).
        admin_token: JWT token for admin operations (queue stats, cleanup).
        timeout: Request timeout in seconds (default: 30.0).
        retries: Number of automatic retries on transient failures (default: 3).

    Example::

        from codeq import CodeQClient

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
    """

    def __init__(
        self,
        base_url: str,
        *,
        producer_token: str | None = None,
        worker_token: str | None = None,
        admin_token: str | None = None,
        timeout: float = 30.0,
        retries: int = 3,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._producer_token = producer_token
        self._worker_token = worker_token
        self._admin_token = admin_token
        self._retries = retries

        self._http = httpx.AsyncClient(
            base_url=self._base_url,
            timeout=timeout,
            headers={"Content-Type": "application/json"},
        )

    async def __aenter__(self) -> CodeQClient:
        return self

    async def __aexit__(self, *args: Any) -> None:
        await self.close()

    async def close(self) -> None:
        """Close the underlying HTTP client and release connection pool resources."""
        await self._http.aclose()

    # ──────────────────────────────────────────────
    # Producer Operations
    # ──────────────────────────────────────────────

    async def create_task(self, options: CreateTaskOptions) -> Task:
        """Create a new task in the queue.

        Requires a producer token. The task is placed into the appropriate queue
        based on its command and becomes available for workers to claim.

        Args:
            options: Task creation options including command, payload, and priority.

        Returns:
            The created task with its assigned ID and initial status.

        Raises:
            CodeQAuthError: If the producer token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._producer_token:
            raise CodeQAuthError("Producer token is required to create tasks")

        data = await self._post(
            "/v1/codeq/tasks",
            json=_serialize_options(options),
            token=self._producer_token,
        )
        return _build_task(data)

    # ──────────────────────────────────────────────
    # Worker Operations
    # ──────────────────────────────────────────────

    async def claim_task(self, options: ClaimTaskOptions) -> Task | None:
        """Claim a task from the queue for processing.

        Requires a worker token with ``codeq:claim`` scope. Supports long-polling
        via ``wait_seconds`` to avoid busy-waiting when no tasks are available.

        Args:
            options: Claim options including command filter and lease duration.

        Returns:
            The claimed task, or ``None`` if no task is available (HTTP 204).

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to claim tasks")

        resp = await self._request(
            "POST",
            "/v1/codeq/tasks/claim",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        if resp.status_code == 204:
            return None
        return _build_task(resp.json())

    async def submit_result(
        self, task_id: str, options: SubmitResultOptions
    ) -> ResultRecord:
        """Submit a result for a completed or failed task.

        Requires a worker token with ``codeq:result`` scope.

        Args:
            task_id: ID of the task to submit a result for.
            options: Result submission options including status and data.

        Returns:
            The result record.

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to submit results")

        data = await self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/result",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        return _build_result_record(data)

    async def heartbeat(self, task_id: str, extend_seconds: int = 300) -> None:
        """Send a heartbeat to extend the lease on a claimed task.

        Requires a worker token with ``codeq:heartbeat`` scope. Workers should
        send heartbeats periodically to prevent lease expiration.

        Args:
            task_id: ID of the task to extend the lease for.
            extend_seconds: Number of seconds to extend the lease (default: 300).

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required for heartbeat")

        await self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/heartbeat",
            json={"extendSeconds": extend_seconds},
            token=self._worker_token,
        )

    async def abandon(self, task_id: str) -> None:
        """Abandon a claimed task, returning it to the queue for another worker.

        Requires a worker token with ``codeq:abandon`` scope.

        Args:
            task_id: ID of the task to abandon.

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to abandon tasks")

        await self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/abandon",
            json={},
            token=self._worker_token,
        )

    async def nack(
        self, task_id: str, delay_seconds: int, reason: str
    ) -> NackResponse:
        """Send a negative acknowledgment (NACK) for a task with optional backoff.

        The task is re-queued with a delay. If the task has exceeded its
        ``max_attempts``, it is moved to the dead letter queue (DLQ).

        Requires a worker token with ``codeq:nack`` scope.

        Args:
            task_id: ID of the task to NACK.
            delay_seconds: Delay in seconds before the task is retried.
            reason: Reason for the negative acknowledgment.

        Returns:
            NACK response indicating whether the task was requeued or moved to DLQ.

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to NACK tasks")

        data = await self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/nack",
            json={"delaySeconds": delay_seconds, "reason": reason},
            token=self._worker_token,
        )
        snake = _camel_to_snake_dict(data)
        return NackResponse(
            status=snake["status"], delay_seconds=snake["delay_seconds"]
        )

    # ──────────────────────────────────────────────
    # Subscription (Webhook) Operations
    # ──────────────────────────────────────────────

    async def create_subscription(
        self, options: CreateSubscriptionOptions
    ) -> SubscriptionResponse:
        """Register a webhook subscription for push-based task delivery.

        Requires a worker token with ``codeq:subscribe`` scope. The server will
        POST task notifications to the specified callback URL.

        Args:
            options: Subscription options including callback URL and event types.

        Returns:
            Subscription response with ID and expiration time.

        Raises:
            CodeQAuthError: If the worker token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to create subscriptions")

        data = await self._post(
            "/v1/codeq/workers/subscriptions",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        snake = _camel_to_snake_dict(data)
        return SubscriptionResponse(
            subscription_id=snake["subscription_id"],
            expires_at=snake["expires_at"],
        )

    async def renew_subscription(
        self,
        subscription_id: str,
        options: RenewSubscriptionOptions | None = None,
    ) -> SubscriptionResponse:
        """Renew an existing webhook subscription to extend its TTL.

        Args:
            subscription_id: ID of the subscription to renew.
            options: Optional renewal options (e.g. new TTL).

        Returns:
            Updated subscription response with new expiration time.

        Raises:
            CodeQAuthError: If no token is available.
            CodeQAPIError: If the API request fails.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to renew subscriptions")

        body = _serialize_options(options) if options else {}
        data = await self._post(
            f"/v1/codeq/workers/subscriptions/{_encode_id(subscription_id)}/heartbeat",
            json=body,
            token=token,
        )
        snake = _camel_to_snake_dict(data)
        return SubscriptionResponse(
            subscription_id=snake["subscription_id"],
            expires_at=snake["expires_at"],
        )

    # ──────────────────────────────────────────────
    # Query Operations
    # ──────────────────────────────────────────────

    async def get_task(self, task_id: str) -> Task:
        """Retrieve a task by its ID.

        Args:
            task_id: ID of the task to retrieve.

        Returns:
            The task details.

        Raises:
            CodeQAuthError: If no token is available.
            CodeQAPIError: If the API request fails.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to get task")

        data = await self._get(
            f"/v1/codeq/tasks/{_encode_id(task_id)}",
            token=token,
        )
        return _build_task(data)

    async def get_result(self, task_id: str) -> TaskResult:
        """Retrieve the result for a completed or failed task.

        Args:
            task_id: ID of the task to get the result for.

        Returns:
            Task result containing both the task and result record.

        Raises:
            CodeQAuthError: If no token is available.
            CodeQAPIError: If the API request fails.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to get result")

        data = await self._get(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/result",
            token=token,
        )
        return TaskResult(
            task=_build_task(data["task"]),
            result=_build_result_record(data["result"]),
        )

    async def wait_for_result(
        self,
        task_id: str,
        options: WaitForResultOptions | None = None,
    ) -> TaskResult:
        """Poll for a task result until it is available or a timeout is reached.

        This is a convenience method that repeatedly calls :meth:`get_result` until
        a result is available.

        Args:
            task_id: ID of the task to wait for.
            options: Polling options (timeout and interval).

        Returns:
            The task result.

        Raises:
            CodeQTimeoutError: If the timeout is reached.
            CodeQAuthError: If no token is available.
            CodeQAPIError: If polling fails.
        """
        opts = options or WaitForResultOptions()
        deadline = asyncio.get_event_loop().time() + opts.timeout

        while asyncio.get_event_loop().time() < deadline:
            try:
                return await self.get_result(task_id)
            except (CodeQAPIError, httpx.HTTPStatusError):
                pass

            remaining = deadline - asyncio.get_event_loop().time()
            if remaining <= 0:
                break

            await asyncio.sleep(min(opts.poll_interval, remaining))

        raise CodeQTimeoutError(
            f"Timed out waiting for result of task {task_id} after {opts.timeout}s"
        )

    # ──────────────────────────────────────────────
    # Admin Operations
    # ──────────────────────────────────────────────

    async def list_queues(self) -> list[QueueStats]:
        """List statistics for all queues.

        Requires an admin token with ``admin`` scope.

        Returns:
            List of queue statistics.

        Raises:
            CodeQAuthError: If the admin token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required to list queues")

        data = await self._get("/v1/codeq/admin/queues", token=self._admin_token)
        result: list[QueueStats] = []
        for item in data:
            snake = _camel_to_snake_dict(item)
            result.append(
                QueueStats(
                    **{
                        k: v
                        for k, v in snake.items()
                        if k in QueueStats.__dataclass_fields__
                    }
                )
            )
        return result

    async def get_queue_stats(self, command: str) -> QueueStats:
        """Get statistics for a specific queue by command name.

        Requires an admin token with ``admin`` scope.

        Args:
            command: Command name to get statistics for.

        Returns:
            Queue statistics for the specified command.

        Raises:
            CodeQAuthError: If the admin token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required to get queue stats")

        data = await self._get(
            f"/v1/codeq/admin/queues/{_encode_id(command)}",
            token=self._admin_token,
        )
        snake = _camel_to_snake_dict(data)
        return QueueStats(
            **{k: v for k, v in snake.items() if k in QueueStats.__dataclass_fields__}
        )

    async def cleanup_expired(
        self, options: CleanupOptions | None = None
    ) -> CleanupResult:
        """Clean up expired tasks.

        Requires an admin token with ``admin`` scope.

        Args:
            options: Cleanup options (limit and cutoff timestamp).

        Returns:
            Cleanup result with the number of deleted tasks.

        Raises:
            CodeQAuthError: If the admin token is missing.
            CodeQAPIError: If the API request fails.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required for cleanup")

        body = _serialize_options(options) if options else {}
        data = await self._post(
            "/v1/codeq/admin/tasks/cleanup",
            json=body,
            token=self._admin_token,
        )
        return CleanupResult(
            deleted=data["deleted"], before=data["before"], limit=data["limit"]
        )

    # ──────────────────────────────────────────────
    # Internal Helpers
    # ──────────────────────────────────────────────

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json: Any = None,
        token: str,
    ) -> httpx.Response:
        """Make an HTTP request with retry logic."""

        @retry(
            retry=retry_if_exception(_is_retryable),
            stop=stop_after_attempt(self._retries + 1),
            wait=wait_exponential(multiplier=0.5, max=10),
            reraise=True,
        )
        async def _do_request() -> httpx.Response:
            resp = await self._http.request(
                method,
                path,
                json=json,
                headers={"Authorization": f"Bearer {token}"},
            )
            resp.raise_for_status()
            return resp

        try:
            return await _do_request()
        except httpx.HTTPStatusError as exc:
            body = None
            try:
                body = exc.response.json()
            except Exception:
                body = exc.response.text
            raise CodeQAPIError(
                f"Request failed",
                status_code=exc.response.status_code,
                response_body=body,
            ) from exc

    async def _get(self, path: str, *, token: str) -> Any:
        """Perform a GET request and return the parsed JSON response."""
        resp = await self._request("GET", path, token=token)
        return resp.json()

    async def _post(self, path: str, *, json: Any, token: str) -> Any:
        """Perform a POST request and return the parsed JSON response."""
        resp = await self._request("POST", path, json=json, token=token)
        if resp.status_code == 204:
            return None
        return resp.json()
