package redis

import (
	"context"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
)

const (
	// Default values for Redis adapter when not provided by config
	defaultMaxAttempts = 5
	defaultTTLSeconds  = 300
)

// taskStorageAdapter adapts repository.TaskRepository to persistence.TaskStorage
type taskStorageAdapter struct {
	repo           repository.TaskRepository
	maxAttempts    int
}

func newTaskStorageAdapter(repo repository.TaskRepository) *taskStorageAdapter {
	return &taskStorageAdapter{
		repo:        repo,
		maxAttempts: defaultMaxAttempts,
	}
}

func (a *taskStorageAdapter) Save(ctx context.Context, task *domain.Task) error {
	// The existing repository doesn't have a direct Save method
	// Tasks are saved implicitly through Enqueue
	return a.EnqueueTask(ctx, task)
}

func (a *taskStorageAdapter) Get(ctx context.Context, id string) (*domain.Task, error) {
	return a.repo.Get(ctx, id)
}

func (a *taskStorageAdapter) Delete(ctx context.Context, id string) error {
	// The existing repository doesn't have a public Delete method
	// This is handled internally through cleanup operations
	return nil
}

func (a *taskStorageAdapter) EnqueueTask(ctx context.Context, task *domain.Task) error {
	// Use the repository's Enqueue method
	// Note: The Task struct doesn't store idempotencyKey as it's only used at enqueue time
	// and is not persisted. The idempotency tracking is handled separately in Redis keys.
	_, err := a.repo.Enqueue(
		ctx,
		task.Command,
		task.Payload,
		task.Priority,
		task.Webhook,
		task.MaxAttempts,
		"", // idempotencyKey - not stored in Task, handled separately by caller
		time.Time{}, // visibleAt - use zero time for immediate visibility
		task.TenantID,
	)
	return err
}

func (a *taskStorageAdapter) ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, tenantID string) (*domain.Task, bool, error) {
	// Use configured max attempts
	return a.repo.Claim(ctx, workerID, commands, leaseSeconds, inspectLimit, a.maxAttempts, tenantID)
}

func (a *taskStorageAdapter) UpdateLease(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	return a.repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
}

func (a *taskStorageAdapter) AbandonLease(ctx context.Context, taskID string, workerID string) error {
	return a.repo.Abandon(ctx, taskID, workerID)
}

func (a *taskStorageAdapter) NackTask(ctx context.Context, taskID string, workerID string, delaySeconds int, reason string) error {
	// The existing Nack method returns attempt count and moved-to-dlq flag
	// We ignore these for the adapter
	_, _, err := a.repo.Nack(ctx, taskID, workerID, delaySeconds, a.maxAttempts, reason)
	return err
}

func (a *taskStorageAdapter) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	return a.repo.MoveDueDelayed(ctx, cmd, limit)
}

func (a *taskStorageAdapter) QueueLength(ctx context.Context, cmd domain.Command) (int64, error) {
	return a.repo.PendingLength(ctx, cmd)
}

func (a *taskStorageAdapter) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	return a.repo.QueueStats(ctx, cmd)
}

func (a *taskStorageAdapter) AdminQueues(ctx context.Context) (map[string]any, error) {
	return a.repo.AdminQueues(ctx)
}

func (a *taskStorageAdapter) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	return a.repo.CleanupExpired(ctx, limit, before)
}

// resultStorageAdapter adapts repository.ResultRepository to persistence.ResultStorage
type resultStorageAdapter struct {
	repo repository.ResultRepository
}

func (a *resultStorageAdapter) SaveResult(ctx context.Context, rec domain.ResultRecord) error {
	return a.repo.SaveResult(ctx, rec)
}

func (a *resultStorageAdapter) GetResult(ctx context.Context, taskID string) (*domain.ResultRecord, error) {
	return a.repo.GetResult(ctx, taskID)
}

func (a *resultStorageAdapter) UpdateTaskOnComplete(ctx context.Context, taskID string, status domain.TaskStatus, errorMsg string) error {
	return a.repo.UpdateTaskOnComplete(ctx, taskID, status, errorMsg)
}

func (a *resultStorageAdapter) RemoveFromInprogAndClearLease(ctx context.Context, taskID string, cmd domain.Command) error {
	return a.repo.RemoveFromInprogAndClearLease(ctx, taskID, cmd)
}

// subscriptionStorageAdapter adapts repository.SubscriptionRepository to persistence.SubscriptionStorage
type subscriptionStorageAdapter struct {
	repo repository.SubscriptionRepository
	ttl  int
}

func newSubscriptionStorageAdapter(repo repository.SubscriptionRepository) *subscriptionStorageAdapter {
	return &subscriptionStorageAdapter{
		repo: repo,
		ttl:  defaultTTLSeconds,
	}
}

func (a *subscriptionStorageAdapter) Register(ctx context.Context, sub *domain.Subscription) error {
	// Use configured TTL
	_, err := a.repo.Create(ctx, *sub, a.ttl)
	return err
}

func (a *subscriptionStorageAdapter) Unregister(ctx context.Context, workerID string, commands []domain.Command) error {
	// The existing repository doesn't have an Unregister method
	// Subscriptions expire via TTL in Redis
	// A proper implementation would require adding this method to the repository
	// For now, this is a no-op as subscriptions auto-expire
	return nil
}

func (a *subscriptionStorageAdapter) GetByCommand(ctx context.Context, commands []domain.Command) ([]*domain.Subscription, error) {
	// Collect subscriptions from all commands
	var allSubs []*domain.Subscription
	for _, cmd := range commands {
		subs, err := a.repo.ListActive(ctx, cmd, time.Now())
		if err != nil {
			return nil, err
		}
		for i := range subs {
			allSubs = append(allSubs, &subs[i])
		}
	}
	return allSubs, nil
}

func (a *subscriptionStorageAdapter) GetByWorker(ctx context.Context, workerID string) ([]*domain.Subscription, error) {
	// The existing repository doesn't have a GetByWorker method
	// The domain.Subscription struct doesn't have a WorkerID field
	// This functionality would require extending both the domain model and repository
	// For now, return not found
	return nil, persistence.ErrNotFound
}

func (a *subscriptionStorageAdapter) RemoveExpired(ctx context.Context, before time.Time) (int, error) {
	return a.repo.CleanupExpired(ctx, 1000, before)
}
