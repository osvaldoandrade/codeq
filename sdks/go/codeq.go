// Package codeq provides a Go client for the CodeQ task scheduling server.
//
// The client supports producer, worker, and admin operations with automatic
// retry logic and configurable timeouts.
//
// Example usage:
//
//	client := codeq.NewClient("http://localhost:8080",
//		codeq.WithProducerToken("your-producer-token"),
//		codeq.WithWorkerToken("your-worker-token"),
//	)
//
//	// Create a task
//	task, err := client.CreateTask(ctx, &codeq.CreateTaskOptions{
//		Command:  "GENERATE_MASTER",
//		Payload:  map[string]any{"jobId": "123"},
//		Priority: codeq.Int(5),
//	})
//
//	// Claim a task
//	claimed, err := client.ClaimTask(ctx, &codeq.ClaimTaskOptions{
//		Commands:     []string{"GENERATE_MASTER"},
//		LeaseSeconds: codeq.Int(120),
//		WaitSeconds:  codeq.Int(10),
//	})
//
//	// Submit result
//	_, err = client.SubmitResult(ctx, claimed.ID, &codeq.SubmitResultOptions{
//		Status: "COMPLETED",
//		Result: map[string]any{"output": "done"},
//	})
package codeq

import "time"

// TaskStatus represents the current state of a task.
type TaskStatus string

const (
	StatusPending    TaskStatus = "PENDING"
	StatusInProgress TaskStatus = "IN_PROGRESS"
	StatusCompleted  TaskStatus = "COMPLETED"
	StatusFailed     TaskStatus = "FAILED"
)

// Task represents a task in the CodeQ system.
type Task struct {
	ID         string     `json:"id"`
	Command    string     `json:"command"`
	Payload    any        `json:"payload"`
	Priority   int        `json:"priority,omitempty"`
	Webhook    string     `json:"webhook,omitempty"`
	Status     TaskStatus `json:"status"`
	WorkerID   string     `json:"workerId,omitempty"`
	LeaseUntil string     `json:"leaseUntil,omitempty"`
	Attempts   int        `json:"attempts,omitempty"`
	MaxAttempts int       `json:"maxAttempts,omitempty"`
	Error      string     `json:"error,omitempty"`
	ResultKey  string     `json:"resultKey,omitempty"`
	TenantID   string     `json:"tenantId,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// CreateTaskOptions specifies parameters for creating a task.
type CreateTaskOptions struct {
	// Command name that identifies the task type (required).
	Command string `json:"command"`
	// Payload is the task data (required).
	Payload any `json:"payload"`
	// Priority sets task priority; higher values are processed first.
	Priority *int `json:"priority,omitempty"`
	// Webhook URL to call on task completion.
	Webhook string `json:"webhook,omitempty"`
	// MaxAttempts sets the maximum number of retry attempts.
	MaxAttempts *int `json:"maxAttempts,omitempty"`
	// DelaySeconds delays the task before it becomes available.
	DelaySeconds *int `json:"delaySeconds,omitempty"`
	// IdempotencyKey for deduplication within a 24h window.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// RunAt is an RFC 3339 timestamp for scheduled execution.
	RunAt string `json:"runAt,omitempty"`
}

// ClaimTaskOptions specifies parameters for claiming a task.
type ClaimTaskOptions struct {
	// Commands to filter tasks by (required).
	Commands []string `json:"commands"`
	// LeaseSeconds sets the lease duration (default: 300).
	LeaseSeconds *int `json:"leaseSeconds,omitempty"`
	// WaitSeconds sets the long-poll wait time (max: 30, default: 0).
	WaitSeconds *int `json:"waitSeconds,omitempty"`
}

// BatchClaimOptions specifies parameters for batch claiming tasks.
type BatchClaimOptions struct {
	// Commands to filter tasks by.
	Commands []string `json:"commands,omitempty"`
	// LeaseSeconds sets the lease duration.
	LeaseSeconds *int `json:"leaseSeconds,omitempty"`
	// Count is the number of tasks to claim (required, max: 10).
	Count int `json:"count"`
}

// ArtifactIn represents an artifact to attach when submitting a result.
type ArtifactIn struct {
	// Name is the artifact filename (required).
	Name string `json:"name"`
	// URL to externally hosted artifact.
	URL string `json:"url,omitempty"`
	// ContentBase64 is base64-encoded inline content.
	ContentBase64 string `json:"contentBase64,omitempty"`
	// ContentType is the MIME content type.
	ContentType string `json:"contentType,omitempty"`
}

// ArtifactOut represents an artifact returned in a result record.
type ArtifactOut struct {
	// Name is the artifact filename.
	Name string `json:"name"`
	// URL to the stored artifact.
	URL string `json:"url"`
}

// SubmitResultOptions specifies parameters for submitting a task result.
type SubmitResultOptions struct {
	// Status is the result status: "COMPLETED" or "FAILED" (required).
	Status string `json:"status"`
	// Result data (should be provided when status is COMPLETED).
	Result map[string]any `json:"result,omitempty"`
	// Error message (required if status is FAILED).
	Error string `json:"error,omitempty"`
	// Artifacts to attach to the result.
	Artifacts []ArtifactIn `json:"artifacts,omitempty"`
}

// ResultRecord represents a stored task result.
type ResultRecord struct {
	TaskID      string         `json:"taskId"`
	Status      TaskStatus     `json:"status"`
	Result      map[string]any `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`
	Artifacts   []ArtifactOut  `json:"artifacts,omitempty"`
	CompletedAt time.Time      `json:"completedAt"`
}

// TaskResult contains both the task and its result.
type TaskResult struct {
	Task   Task         `json:"task"`
	Result ResultRecord `json:"result"`
}

// NackResponse contains the result of a NACK operation.
type NackResponse struct {
	// Status is "requeued" or "dlq".
	Status string `json:"status"`
	// DelaySeconds before the task is retried.
	DelaySeconds int `json:"delaySeconds"`
}

// QueueStats contains statistics for a command queue.
type QueueStats struct {
	Command    string `json:"command"`
	Ready      int64  `json:"ready"`
	Delayed    int64  `json:"delayed"`
	InProgress int64  `json:"inProgress"`
	DLQ        int64  `json:"dlq"`
}

// CreateSubscriptionOptions specifies parameters for creating a webhook subscription.
type CreateSubscriptionOptions struct {
	// CallbackURL is the webhook endpoint (required).
	CallbackURL string `json:"callbackUrl"`
	// EventTypes are the command types to subscribe to.
	EventTypes []string `json:"eventTypes,omitempty"`
	// TTLSeconds sets the subscription time-to-live (default: 300).
	TTLSeconds *int `json:"ttlSeconds,omitempty"`
	// DeliveryMode: "fanout", "group", or "hash".
	DeliveryMode string `json:"deliveryMode,omitempty"`
	// GroupID is required if DeliveryMode is "group".
	GroupID string `json:"groupId,omitempty"`
	// MinIntervalSeconds between deliveries.
	MinIntervalSeconds *int `json:"minIntervalSeconds,omitempty"`
}

// SubscriptionResponse contains the result of a subscription operation.
type SubscriptionResponse struct {
	SubscriptionID string `json:"subscriptionId"`
	ExpiresAt      string `json:"expiresAt"`
}

// RenewSubscriptionOptions specifies parameters for renewing a subscription.
type RenewSubscriptionOptions struct {
	// TTLSeconds sets the new time-to-live.
	TTLSeconds *int `json:"ttlSeconds,omitempty"`
}

// WaitForResultOptions configures the polling behavior of WaitForResult.
type WaitForResultOptions struct {
	// Timeout is the maximum time to wait (default: 30s).
	Timeout time.Duration
	// PollInterval is the time between polls (default: 1s).
	PollInterval time.Duration
}

// CleanupOptions specifies parameters for admin task cleanup.
type CleanupOptions struct {
	// Limit is the maximum number of tasks to clean up (default: 1000).
	Limit *int `json:"limit,omitempty"`
	// Before is an RFC 3339 timestamp cutoff (default: now).
	Before string `json:"before,omitempty"`
}

// CleanupResult contains the result of a cleanup operation.
type CleanupResult struct {
	Deleted int    `json:"deleted"`
	Before  string `json:"before"`
	Limit   int    `json:"limit"`
}

// BatchCreateResult contains the result for a single task in a batch create.
type BatchCreateResult struct {
	Task  *Task  `json:"task,omitempty"`
	Error string `json:"error,omitempty"`
}

// BatchSubmitItem specifies a single result in a batch submission.
type BatchSubmitItem struct {
	TaskID string `json:"taskId"`
	SubmitResultOptions
}

// BatchSubmitResult contains the result for a single item in a batch submit.
type BatchSubmitResult struct {
	TaskID string        `json:"taskId"`
	Result *ResultRecord `json:"result,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Int returns a pointer to the given int value. Useful for optional int fields.
func Int(v int) *int { return &v }
