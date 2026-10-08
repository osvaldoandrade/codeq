package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TaskRepository is the queue storage contract. The server implements it
// with Pebble. Callers depend on this interface, not on a backend.
type TaskRepository interface {
	Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey, taskID string, visibleAt time.Time, tenantID string) (*domain.Task, error)
	// EnqueueWithReady behaves like Enqueue but also reports whether this insert just
	// transitioned the immediate pending queue from empty to non-empty.
	EnqueueWithReady(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey, taskID string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error)
	Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error)
	Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error
	// Progress stores the progress value reported by the worker holding the
	// lease of an in-progress task. It fails with "not-found", "not-owner"
	// or "not-in-progress" like the other lease-holder operations.
	Progress(ctx context.Context, taskID string, workerID string, progress json.RawMessage) error
	Abandon(ctx context.Context, taskID string, workerID string) error
	Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error)
	MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error)
	PendingLength(ctx context.Context, cmd domain.Command) (int64, error)
	Get(ctx context.Context, taskID string) (*domain.Task, error)
	AdminQueues(ctx context.Context) (map[string]any, error)
	QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error)
	// ListTasks returns up to limit tasks of one (cmd, tenant) queue state,
	// resuming after cursor (empty for the first page). See ADR 0005.
	ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error)
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

// ResultRepository stores task results and the completion transition.
type ResultRepository interface {
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error)
	SaveResult(ctx context.Context, rec domain.ResultRecord, cmd domain.Command, tenantID string) error
	GetResult(ctx context.Context, id string) (*domain.ResultRecord, error)
	UpdateTaskOnComplete(ctx context.Context, id string, cmd domain.Command, tenantID string, status domain.TaskStatus, errorMsg string) error
	RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command, tenantID string) error
	DecodeBase64(s string) ([]byte, error)
	GetTasksBatch(ctx context.Context, ids []string) (map[string]*domain.Task, error)
	BatchUpdateTasksOnComplete(ctx context.Context, updates []domain.TaskCompleteUpdate) error
	BatchRemoveFromInprogAndClearLease(ctx context.Context, deletes []domain.TaskDeleteInfo) error
}

// SubscriptionRepository stores worker subscriptions for queue-ready notifications.
type SubscriptionRepository interface {
	Create(ctx context.Context, sub domain.Subscription, ttlSeconds int) (*domain.Subscription, error)
	Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error)
	Get(ctx context.Context, id string) (*domain.Subscription, error)
	ListActive(ctx context.Context, cmd domain.Command, now time.Time) ([]domain.Subscription, error)
	HasActive(ctx context.Context, cmd domain.Command) bool
	AllowNotify(ctx context.Context, id string, minIntervalSeconds int) (bool, error)
	AllowNotifyBatch(ctx context.Context, subs []domain.Subscription) (map[string]bool, error)
	NextGroupIndex(ctx context.Context, cmd domain.Command, groupID string, mod int) (int, error)
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}
