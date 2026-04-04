// Package codeq provides a Go client SDK for the codeQ task-queue API.
package codeq

import "time"

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	// StatusPending indicates the task is waiting to be claimed by a worker.
	StatusPending TaskStatus = "PENDING"
	// StatusInProgress indicates the task has been claimed and is being processed.
	StatusInProgress TaskStatus = "IN_PROGRESS"
	// StatusCompleted indicates the task finished successfully.
	StatusCompleted TaskStatus = "COMPLETED"
	// StatusFailed indicates the task finished with an error.
	StatusFailed TaskStatus = "FAILED"
)

// Task represents a codeQ task as returned by the API.
type Task struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Payload     any        `json:"payload"`
	Priority    int        `json:"priority"`
	Webhook     string     `json:"webhook,omitempty"`
	Status      TaskStatus `json:"status"`
	WorkerID    string     `json:"workerId,omitempty"`
	LeaseUntil  string     `json:"leaseUntil,omitempty"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"maxAttempts"`
	Error       string     `json:"error,omitempty"`
	ResultKey   string     `json:"resultKey,omitempty"`
	TenantID    string     `json:"tenantId,omitempty"`
	CreatedAt   string     `json:"createdAt,omitempty"`
	UpdatedAt   string     `json:"updatedAt,omitempty"`
}

// CreateTaskOptions configures the creation of a new task.
type CreateTaskOptions struct {
	// Command is the task command identifier (required).
	Command string `json:"command"`
	// Payload is the arbitrary data passed to the worker.
	Payload any `json:"payload"`
	// Priority controls execution order; higher values run first.
	// When nil the server default is used.
	Priority *int `json:"priority,omitempty"`
	// Webhook is an optional URL to notify on task completion.
	Webhook string `json:"webhook,omitempty"`
	// MaxAttempts caps the number of delivery attempts. When nil the server
	// default is used.
	MaxAttempts *int `json:"maxAttempts,omitempty"`
	// DelaySeconds postpones the task's visibility in the queue.
	DelaySeconds *int `json:"delaySeconds,omitempty"`
	// IdempotencyKey prevents duplicate task creation for the same logical
	// operation.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// RunAt schedules the task for a specific time (RFC 3339).
	RunAt string `json:"runAt,omitempty"`
}

// ClaimTaskOptions configures how a worker claims a task from the queue.
type ClaimTaskOptions struct {
	// Commands lists the command types the worker can handle (required).
	Commands []string `json:"commands"`
	// LeaseSeconds sets how long the worker holds the task before it becomes
	// reclaimable. When nil the server default is used.
	LeaseSeconds *int `json:"leaseSeconds,omitempty"`
	// WaitSeconds enables long-polling; the server holds the request open for
	// up to this many seconds when no task is immediately available.
	WaitSeconds *int `json:"waitSeconds,omitempty"`
}

// SubmitResultOptions configures the result submission for a completed task.
type SubmitResultOptions struct {
	// Status must be StatusCompleted or StatusFailed.
	Status TaskStatus `json:"status"`
	// Result is the arbitrary output data produced by the worker.
	Result any `json:"result,omitempty"`
	// Error is a human-readable error description (used when Status is
	// StatusFailed).
	Error string `json:"error,omitempty"`
	// Artifacts are optional files or references attached to the result.
	Artifacts []ArtifactIn `json:"artifacts,omitempty"`
}

// ArtifactIn describes an artifact attached to a result submission.
type ArtifactIn struct {
	// Name is the display name of the artifact (required).
	Name string `json:"name"`
	// URL points to an externally-hosted artifact.
	URL string `json:"url,omitempty"`
	// ContentBase64 is the base64-encoded artifact content for inline upload.
	ContentBase64 string `json:"contentBase64,omitempty"`
	// ContentType is the MIME type of the artifact (e.g. "application/pdf").
	ContentType string `json:"contentType,omitempty"`
}

// ArtifactOut describes an artifact returned from a result query.
type ArtifactOut struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ResultRecord represents the stored result of a completed task.
type ResultRecord struct {
	TaskID      string        `json:"taskId"`
	Status      TaskStatus    `json:"status"`
	Result      any           `json:"result,omitempty"`
	Error       string        `json:"error,omitempty"`
	Artifacts   []ArtifactOut `json:"artifacts,omitempty"`
	CompletedAt string        `json:"completedAt,omitempty"`
}

// TaskResult pairs a task with its optional result payload.
type TaskResult struct {
	Task   Task `json:"task"`
	Result any  `json:"result,omitempty"`
}

// NackResponse is returned when a worker negatively acknowledges a task,
// causing it to be retried after a delay.
type NackResponse struct {
	Status       string `json:"status"`
	DelaySeconds int    `json:"delaySeconds"`
}

// QueueStats holds real-time statistics for a single command queue.
type QueueStats struct {
	Command    string `json:"command"`
	Ready      int64  `json:"ready"`
	Delayed    int64  `json:"delayed"`
	InProgress int64  `json:"inProgress"`
	DLQ        int64  `json:"dlq"`
}

// CreateSubscriptionOptions configures the creation of a webhook
// subscription.
type CreateSubscriptionOptions struct {
	// CallbackURL is the endpoint that receives event notifications
	// (required).
	CallbackURL string `json:"callbackUrl"`
	// EventTypes restricts which events trigger the callback.
	EventTypes []string `json:"eventTypes,omitempty"`
	// TTLSeconds sets the subscription lifetime. When nil the server default
	// is used.
	TTLSeconds *int `json:"ttlSeconds,omitempty"`
	// DeliveryMode controls how events are delivered (e.g. "push" or "poll").
	DeliveryMode string `json:"deliveryMode,omitempty"`
	// GroupID groups subscriptions so only one in the group is notified.
	GroupID string `json:"groupId,omitempty"`
	// MinIntervalSeconds throttles callback frequency.
	MinIntervalSeconds *int `json:"minIntervalSeconds,omitempty"`
}

// SubscriptionResponse is returned by subscription create and renew
// operations.
type SubscriptionResponse struct {
	SubscriptionID string `json:"subscriptionId"`
	ExpiresAt      string `json:"expiresAt"`
}

// RenewSubscriptionOptions configures the renewal of an existing webhook
// subscription.
type RenewSubscriptionOptions struct {
	// TTLSeconds extends the subscription lifetime. When nil the server
	// default is used.
	TTLSeconds *int `json:"ttlSeconds,omitempty"`
}

// WaitForResultOptions configures client-side polling when waiting for a task
// result.
type WaitForResultOptions struct {
	// Timeout is the maximum time to wait for a result.
	// Default: 30s.
	Timeout time.Duration
	// PollInterval is the pause between successive poll requests.
	// Default: 1s.
	PollInterval time.Duration
}

// CleanupOptions configures the removal of expired or old tasks.
type CleanupOptions struct {
	// Limit caps the number of tasks deleted in a single call.
	Limit *int `json:"limit,omitempty"`
	// Before deletes only tasks created before this timestamp (RFC 3339).
	Before string `json:"before,omitempty"`
}

// CleanupResult is returned by cleanup operations.
type CleanupResult struct {
	Deleted int    `json:"deleted"`
	Before  string `json:"before,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// BatchTaskResult holds the outcome of a single task within a batch
// operation.
type BatchTaskResult struct {
	// Task is populated on success.
	Task *Task `json:"task,omitempty"`
	// Error is populated when the individual task operation failed.
	Error string `json:"error,omitempty"`
}

// BatchResultSubmission pairs a task ID with the result to submit, used in
// batch result submission requests.
type BatchResultSubmission struct {
	TaskID  string              `json:"taskId"`
	Options SubmitResultOptions `json:"options"`
}
