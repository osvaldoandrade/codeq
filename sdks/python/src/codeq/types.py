"""Type definitions for the CodeQ Python SDK."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Literal


class TaskStatus(str, Enum):
    """Task status enumeration."""

    PENDING = "PENDING"
    IN_PROGRESS = "IN_PROGRESS"
    COMPLETED = "COMPLETED"
    FAILED = "FAILED"


@dataclass
class Task:
    """Represents a task in the CodeQ system."""

    id: str
    """Unique task identifier (UUID)."""

    command: str
    """Command name that identifies the task type."""

    payload: dict[str, Any]
    """Task payload as a JSON object."""

    status: TaskStatus
    """Current task status."""

    created_at: str
    """RFC 3339 timestamp when the task was created."""

    updated_at: str
    """RFC 3339 timestamp when the task was last updated."""

    priority: int | None = None
    """Task priority (higher values are processed first)."""

    webhook: str | None = None
    """Optional webhook URL called on task completion."""

    worker_id: str | None = None
    """ID of the worker that claimed this task."""

    lease_until: str | None = None
    """RFC 3339 timestamp when the lease expires."""

    attempts: int | None = None
    """Number of processing attempts."""

    max_attempts: int | None = None
    """Maximum number of retry attempts."""

    error: str | None = None
    """Error message if the task failed."""

    result_key: str | None = None
    """Key referencing stored result data."""

    tenant_id: str | None = None
    """Tenant identifier for multi-tenant isolation."""


@dataclass
class CreateTaskOptions:
    """Options for creating a task."""

    command: str
    """Command name that identifies the task type."""

    payload: dict[str, Any]
    """Task payload (arbitrary JSON object)."""

    priority: int | None = None
    """Task priority (higher values are processed first, default: 0)."""

    webhook: str | None = None
    """Webhook URL to call on task completion."""

    max_attempts: int | None = None
    """Maximum number of retry attempts (default: 5)."""

    delay_seconds: int | None = None
    """Delay in seconds before the task becomes available."""

    idempotency_key: str | None = None
    """Idempotency key for deduplication (24h window)."""

    run_at: str | None = None
    """RFC 3339 timestamp for scheduled execution."""


@dataclass
class ClaimTaskOptions:
    """Options for claiming a task."""

    commands: list[str]
    """Command names to filter tasks by."""

    lease_seconds: int = 300
    """Lease duration in seconds (default: 300)."""

    wait_seconds: int = 0
    """Long-poll wait time in seconds (max: 30, default: 0)."""


@dataclass
class ArtifactIn:
    """Artifact input for result submission."""

    name: str
    """Artifact filename."""

    url: str | None = None
    """URL to externally hosted artifact."""

    content_base64: str | None = None
    """Base64-encoded inline content."""

    content_type: str | None = None
    """MIME content type."""


@dataclass
class ArtifactOut:
    """Artifact output from a task result."""

    name: str
    """Artifact filename."""

    url: str
    """URL to the stored artifact."""


@dataclass
class SubmitResultOptions:
    """Options for submitting a task result."""

    status: Literal["COMPLETED", "FAILED"]
    """Result status."""

    result: dict[str, Any] | None = None
    """Result data (should be provided when status is COMPLETED)."""

    error: str | None = None
    """Error message (required if status is FAILED)."""

    artifacts: list[ArtifactIn] | None = None
    """Optional artifacts to attach."""


@dataclass
class ResultRecord:
    """Response returned after submitting a result."""

    task_id: str
    """ID of the associated task."""

    status: TaskStatus
    """Result status."""

    completed_at: str
    """RFC 3339 timestamp when the result was recorded."""

    result: dict[str, Any] | None = None
    """Result data."""

    error: str | None = None
    """Error message if failed."""

    artifacts: list[ArtifactOut] | None = None
    """Output artifacts."""


@dataclass
class TaskResult:
    """Response from get_result containing both task and result."""

    task: Task
    """Full task object."""

    result: ResultRecord
    """Result record."""


@dataclass
class NackResponse:
    """Response from a NACK operation."""

    status: str
    """New task status: 'requeued' or 'dlq'."""

    delay_seconds: int
    """Delay in seconds before retry."""


@dataclass
class QueueStats:
    """Queue statistics for a command."""

    command: str
    """Command name."""

    ready: int
    """Number of tasks ready for processing."""

    delayed: int
    """Number of delayed tasks."""

    in_progress: int
    """Number of tasks currently being processed."""

    dlq: int
    """Number of tasks in the dead letter queue."""


@dataclass
class CreateSubscriptionOptions:
    """Options for creating a webhook subscription."""

    callback_url: str
    """Webhook callback URL."""

    event_types: list[str] | None = None
    """Command types to subscribe to."""

    ttl_seconds: int | None = None
    """Time-to-live in seconds (default: 300)."""

    delivery_mode: Literal["fanout", "group", "hash"] | None = None
    """Delivery mode."""

    group_id: str | None = None
    """Group identifier (required if delivery_mode is 'group')."""

    min_interval_seconds: int | None = None
    """Minimum interval between deliveries in seconds."""


@dataclass
class SubscriptionResponse:
    """Response from subscription creation or renewal."""

    subscription_id: str
    """Unique subscription identifier."""

    expires_at: str
    """RFC 3339 timestamp when the subscription expires."""


@dataclass
class RenewSubscriptionOptions:
    """Options for renewing a subscription."""

    ttl_seconds: int | None = None
    """New TTL in seconds."""


@dataclass
class WaitForResultOptions:
    """Options for the wait_for_result polling method."""

    timeout: float = 30.0
    """Maximum time to wait in seconds (default: 30.0)."""

    poll_interval: float = 1.0
    """Polling interval in seconds (default: 1.0)."""


@dataclass
class CleanupOptions:
    """Options for admin task cleanup."""

    limit: int | None = None
    """Maximum number of tasks to clean up (default: 1000)."""

    before: str | None = None
    """RFC 3339 timestamp cutoff for cleanup (default: now)."""


@dataclass
class CleanupResult:
    """Response from admin task cleanup."""

    deleted: int
    """Number of tasks deleted."""

    before: str
    """Cutoff timestamp used."""

    limit: int
    """Limit that was applied."""


@dataclass
class BatchCreateResult:
    """Result for a single task in a batch create response."""

    task: Task | None = None
    """The created task (present on success)."""

    error: str | None = None
    """Error message (present on failure)."""


@dataclass
class BatchClaimOptions:
    """Options for claiming multiple tasks in a single request."""

    count: int
    """Number of tasks to claim (max: 10)."""

    commands: list[str] | None = None
    """Command names to filter tasks by."""

    lease_seconds: int | None = None
    """Lease duration in seconds (default: 300)."""


@dataclass
class BatchSubmitItem:
    """A single result item for batch result submission."""

    task_id: str
    """ID of the task to submit a result for."""

    status: Literal["COMPLETED", "FAILED"]
    """Result status."""

    result: dict[str, Any] | None = None
    """Result data (should be provided when status is COMPLETED)."""

    error: str | None = None
    """Error message (required if status is FAILED)."""

    artifacts: list[ArtifactIn] | None = None
    """Optional artifacts to attach."""


@dataclass
class BatchSubmitResult:
    """Result for a single item in a batch submit response."""

    task_id: str
    """ID of the associated task."""

    result: ResultRecord | None = None
    """The result record (present on success)."""

    error: str | None = None
    """Error message (present on failure)."""
