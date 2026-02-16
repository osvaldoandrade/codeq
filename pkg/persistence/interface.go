package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

var (
	// ErrNotFound is returned when a key does not exist
	ErrNotFound = errors.New("not found")
	
	// ErrAlreadyExists is returned when a key already exists
	ErrAlreadyExists = errors.New("already exists")
)

// PluginPersistence provides storage operations for persistence plugins.
// This is the main interface that all persistence backends must implement.
type PluginPersistence interface {
	// TaskStorage returns the task storage implementation
	TaskStorage() TaskStorage
	
	// ResultStorage returns the result storage implementation
	ResultStorage() ResultStorage
	
	// SubscriptionStorage returns the subscription storage implementation
	SubscriptionStorage() SubscriptionStorage
	
	// Health checks if the persistence backend is healthy
	Health(ctx context.Context) error
	
	// Close releases resources held by the persistence backend
	Close() error
}

// TaskStorage defines persistence operations for tasks
type TaskStorage interface {
	// Save saves a task to storage
	Save(ctx context.Context, task *domain.Task) error
	
	// Get retrieves a task by ID
	Get(ctx context.Context, id string) (*domain.Task, error)
	
	// Delete removes a task from storage
	Delete(ctx context.Context, id string) error
	
	// EnqueueTask adds a task to the appropriate queue based on command and priority
	EnqueueTask(ctx context.Context, task *domain.Task) error
	
	// ClaimTask atomically claims the next available task from specified queues
	ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, tenantID string) (*domain.Task, bool, error)
	
	// UpdateLease extends or abandons a task lease
	UpdateLease(ctx context.Context, taskID string, workerID string, extendSeconds int) error
	
	// AbandonLease releases a task lease, making it available for other workers
	AbandonLease(ctx context.Context, taskID string, workerID string) error
	
	// NackTask returns a task to the queue with delay (for retries)
	NackTask(ctx context.Context, taskID string, workerID string, delaySeconds int, reason string) error
	
	// MoveDueDelayed moves delayed tasks that are now ready to pending queues
	MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error)
	
	// QueueLength returns the number of pending tasks for a command
	QueueLength(ctx context.Context, cmd domain.Command) (int64, error)
	
	// QueueStats returns detailed statistics for a queue
	QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error)
	
	// AdminQueues returns administrative view of all queues
	AdminQueues(ctx context.Context) (map[string]any, error)
	
	// CleanupExpired removes expired tasks
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

// ResultStorage defines persistence operations for task results
type ResultStorage interface {
	// SaveResult stores a task result
	SaveResult(ctx context.Context, rec domain.ResultRecord) error
	
	// GetResult retrieves a task result by task ID
	GetResult(ctx context.Context, taskID string) (*domain.ResultRecord, error)
	
	// UpdateTaskOnComplete updates task status when completed
	UpdateTaskOnComplete(ctx context.Context, taskID string, status domain.TaskStatus, errorMsg string) error
	
	// RemoveFromInprogAndClearLease removes task from in-progress and clears lease
	RemoveFromInprogAndClearLease(ctx context.Context, taskID string, cmd domain.Command) error
}

// SubscriptionStorage defines persistence operations for worker subscriptions
type SubscriptionStorage interface {
	// Register saves a subscription
	Register(ctx context.Context, sub *domain.Subscription) error
	
	// Unregister removes a subscription
	Unregister(ctx context.Context, workerID string, commands []domain.Command) error
	
	// GetByCommand retrieves subscriptions for specific commands
	GetByCommand(ctx context.Context, commands []domain.Command) ([]*domain.Subscription, error)
	
	// GetByWorker retrieves all subscriptions for a worker
	GetByWorker(ctx context.Context, workerID string) ([]*domain.Subscription, error)
	
	// RemoveExpired removes expired subscriptions
	RemoveExpired(ctx context.Context, before time.Time) (int, error)
}
