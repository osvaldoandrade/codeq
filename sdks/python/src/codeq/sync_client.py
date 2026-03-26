"""Synchronous CodeQ client implementation."""

from __future__ import annotations

import time
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

from .client import (
    _build_result_record,
    _build_task,
    _camel_to_snake_dict,
    _encode_id,
    _is_retryable,
    _serialize_options,
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


class SyncCodeQClient:
    """Synchronous client for the CodeQ reactive task scheduling system.

    Provides a blocking API for interacting with the CodeQ server. This is a
    convenience wrapper for applications that do not use ``asyncio``.

    The API mirrors :class:`~codeq.client.CodeQClient` exactly, but all methods
    are synchronous.

    Args:
        base_url: Base URL of the CodeQ server (e.g. ``"http://localhost:8080"``).
        producer_token: JWT token for producer operations.
        worker_token: JWT token for worker operations.
        admin_token: JWT token for admin operations.
        timeout: Request timeout in seconds (default: 30.0).
        retries: Number of automatic retries on transient failures (default: 3).

    Example::

        from codeq import SyncCodeQClient

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

        self._http = httpx.Client(
            base_url=self._base_url,
            timeout=timeout,
            headers={"Content-Type": "application/json"},
        )

    def __enter__(self) -> SyncCodeQClient:
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP client and release connection pool resources."""
        self._http.close()

    # ──────────────────────────────────────────────
    # Producer Operations
    # ──────────────────────────────────────────────

    def create_task(self, options: CreateTaskOptions) -> Task:
        """Create a new task in the queue.

        See :meth:`CodeQClient.create_task` for full documentation.
        """
        if not self._producer_token:
            raise CodeQAuthError("Producer token is required to create tasks")

        data = self._post(
            "/v1/codeq/tasks",
            json=_serialize_options(options),
            token=self._producer_token,
        )
        return _build_task(data)

    # ──────────────────────────────────────────────
    # Worker Operations
    # ──────────────────────────────────────────────

    def claim_task(self, options: ClaimTaskOptions) -> Task | None:
        """Claim a task from the queue for processing.

        See :meth:`CodeQClient.claim_task` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to claim tasks")

        resp = self._request(
            "POST",
            "/v1/codeq/tasks/claim",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        if resp.status_code == 204:
            return None
        return _build_task(resp.json())

    def submit_result(
        self, task_id: str, options: SubmitResultOptions
    ) -> ResultRecord:
        """Submit a result for a completed or failed task.

        See :meth:`CodeQClient.submit_result` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to submit results")

        data = self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/result",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        return _build_result_record(data)

    def heartbeat(self, task_id: str, extend_seconds: int = 300) -> None:
        """Send a heartbeat to extend the lease on a claimed task.

        See :meth:`CodeQClient.heartbeat` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required for heartbeat")

        self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/heartbeat",
            json={"extendSeconds": extend_seconds},
            token=self._worker_token,
        )

    def abandon(self, task_id: str) -> None:
        """Abandon a claimed task, returning it to the queue.

        See :meth:`CodeQClient.abandon` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to abandon tasks")

        self._post(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/abandon",
            json={},
            token=self._worker_token,
        )

    def nack(self, task_id: str, delay_seconds: int, reason: str) -> NackResponse:
        """Send a negative acknowledgment (NACK) for a task.

        See :meth:`CodeQClient.nack` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to NACK tasks")

        data = self._post(
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

    def create_subscription(
        self, options: CreateSubscriptionOptions
    ) -> SubscriptionResponse:
        """Register a webhook subscription for push-based task delivery.

        See :meth:`CodeQClient.create_subscription` for full documentation.
        """
        if not self._worker_token:
            raise CodeQAuthError("Worker token is required to create subscriptions")

        data = self._post(
            "/v1/codeq/workers/subscriptions",
            json=_serialize_options(options),
            token=self._worker_token,
        )
        snake = _camel_to_snake_dict(data)
        return SubscriptionResponse(
            subscription_id=snake["subscription_id"],
            expires_at=snake["expires_at"],
        )

    def renew_subscription(
        self,
        subscription_id: str,
        options: RenewSubscriptionOptions | None = None,
    ) -> SubscriptionResponse:
        """Renew an existing webhook subscription to extend its TTL.

        See :meth:`CodeQClient.renew_subscription` for full documentation.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to renew subscriptions")

        body = _serialize_options(options) if options else {}
        data = self._post(
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

    def get_task(self, task_id: str) -> Task:
        """Retrieve a task by its ID.

        See :meth:`CodeQClient.get_task` for full documentation.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to get task")

        data = self._get(
            f"/v1/codeq/tasks/{_encode_id(task_id)}",
            token=token,
        )
        return _build_task(data)

    def get_result(self, task_id: str) -> TaskResult:
        """Retrieve the result for a completed or failed task.

        See :meth:`CodeQClient.get_result` for full documentation.
        """
        token = self._worker_token or self._producer_token
        if not token:
            raise CodeQAuthError("Token is required to get result")

        data = self._get(
            f"/v1/codeq/tasks/{_encode_id(task_id)}/result",
            token=token,
        )
        return TaskResult(
            task=_build_task(data["task"]),
            result=_build_result_record(data["result"]),
        )

    def wait_for_result(
        self,
        task_id: str,
        options: WaitForResultOptions | None = None,
    ) -> TaskResult:
        """Poll for a task result until it is available or a timeout is reached.

        See :meth:`CodeQClient.wait_for_result` for full documentation.
        """
        opts = options or WaitForResultOptions()
        deadline = time.monotonic() + opts.timeout

        while time.monotonic() < deadline:
            try:
                return self.get_result(task_id)
            except (CodeQAPIError, httpx.HTTPStatusError):
                pass

            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break

            time.sleep(min(opts.poll_interval, remaining))

        raise CodeQTimeoutError(
            f"Timed out waiting for result of task {task_id} after {opts.timeout}s"
        )

    # ──────────────────────────────────────────────
    # Admin Operations
    # ──────────────────────────────────────────────

    def list_queues(self) -> list[QueueStats]:
        """List statistics for all queues.

        See :meth:`CodeQClient.list_queues` for full documentation.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required to list queues")

        data = self._get("/v1/codeq/admin/queues", token=self._admin_token)
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

    def get_queue_stats(self, command: str) -> QueueStats:
        """Get statistics for a specific queue by command name.

        See :meth:`CodeQClient.get_queue_stats` for full documentation.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required to get queue stats")

        data = self._get(
            f"/v1/codeq/admin/queues/{_encode_id(command)}",
            token=self._admin_token,
        )
        snake = _camel_to_snake_dict(data)
        return QueueStats(
            **{k: v for k, v in snake.items() if k in QueueStats.__dataclass_fields__}
        )

    def cleanup_expired(
        self, options: CleanupOptions | None = None
    ) -> CleanupResult:
        """Clean up expired tasks.

        See :meth:`CodeQClient.cleanup_expired` for full documentation.
        """
        if not self._admin_token:
            raise CodeQAuthError("Admin token is required for cleanup")

        body = _serialize_options(options) if options else {}
        data = self._post(
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

    def _request(
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
        def _do_request() -> httpx.Response:
            resp = self._http.request(
                method,
                path,
                json=json,
                headers={"Authorization": f"Bearer {token}"},
            )
            resp.raise_for_status()
            return resp

        try:
            return _do_request()
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

    def _get(self, path: str, *, token: str) -> Any:
        """Perform a GET request and return the parsed JSON response."""
        resp = self._request("GET", path, token=token)
        return resp.json()

    def _post(self, path: str, *, json: Any, token: str) -> Any:
        """Perform a POST request and return the parsed JSON response."""
        resp = self._request("POST", path, json=json, token=token)
        if resp.status_code == 204:
            return None
        return resp.json()
