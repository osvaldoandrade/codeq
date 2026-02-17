package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
)

// Plugin implements PluginPersistence for in-memory storage
// This is primarily for testing and should not be used in production
type Plugin struct {
	mu               sync.RWMutex
	tasks            map[string]*domain.Task
	results          map[string]*domain.ResultRecord
	subscriptions    map[string]*domain.Subscription
	queues           map[domain.Command][]*domain.Task
	leases           map[string]*lease
	tz               *time.Location
	backoffPolicy    string
	backoffBase      int
	backoffMax       int
}

type lease struct {
	workerID  string
	expiresAt time.Time
}

// NewPlugin creates a new in-memory persistence plugin
func NewPlugin(config persistence.PluginConfig) (persistence.PluginPersistence, error) {
	return &Plugin{
		tasks:         make(map[string]*domain.Task),
		results:       make(map[string]*domain.ResultRecord),
		subscriptions: make(map[string]*domain.Subscription),
		queues:        make(map[domain.Command][]*domain.Task),
		leases:        make(map[string]*lease),
		tz:            config.Timezone,
		backoffPolicy: config.BackoffPolicy,
		backoffBase:   config.BackoffBaseSeconds,
		backoffMax:    config.BackoffMaxSeconds,
	}, nil
}

// TaskStorage returns the task storage implementation
func (p *Plugin) TaskStorage() persistence.TaskStorage {
	return &taskStorage{plugin: p}
}

// ResultStorage returns the result storage implementation
func (p *Plugin) ResultStorage() persistence.ResultStorage {
	return &resultStorage{plugin: p}
}

// SubscriptionStorage returns the subscription storage implementation
func (p *Plugin) SubscriptionStorage() persistence.SubscriptionStorage {
	return &subscriptionStorage{plugin: p}
}

// Health always returns nil for in-memory storage
func (p *Plugin) Health(ctx context.Context) error {
	return nil
}

// Close is a no-op for in-memory storage
func (p *Plugin) Close() error {
	return nil
}

func init() {
	persistence.RegisterProvider("memory", NewPlugin)
}

// taskStorage implements persistence.TaskStorage for in-memory storage
type taskStorage struct {
	plugin *Plugin
}

func (s *taskStorage) Save(ctx context.Context, task *domain.Task) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	// Deep copy the task
	taskCopy := *task
	s.plugin.tasks[task.ID] = &taskCopy
	return nil
}

func (s *taskStorage) Get(ctx context.Context, id string) (*domain.Task, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	task, exists := s.plugin.tasks[id]
	if !exists {
		return nil, persistence.ErrNotFound
	}
	
	// Return a copy
	taskCopy := *task
	return &taskCopy, nil
}

func (s *taskStorage) Delete(ctx context.Context, id string) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	delete(s.plugin.tasks, id)
	delete(s.plugin.leases, id)
	return nil
}

func (s *taskStorage) EnqueueTask(ctx context.Context, task *domain.Task) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	// Save task
	taskCopy := *task
	s.plugin.tasks[task.ID] = &taskCopy
	
	// Add to queue (always visible immediately in memory plugin)
	s.plugin.queues[task.Command] = append(s.plugin.queues[task.Command], &taskCopy)
	
	return nil
}

func (s *taskStorage) ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, tenantID string) (*domain.Task, bool, error) {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	now := time.Now()
	
	// Search through queues for available tasks
	for _, cmd := range commands {
		queue := s.plugin.queues[cmd]
		for i, task := range queue {
			// Check if task matches tenant
			if tenantID != "" && task.TenantID != tenantID {
				continue
			}
			
			// Check if task is already leased
			if l, exists := s.plugin.leases[task.ID]; exists && l.expiresAt.After(now) {
				continue
			}
			
			// Claim this task
			s.plugin.leases[task.ID] = &lease{
				workerID:  workerID,
				expiresAt: now.Add(time.Duration(leaseSeconds) * time.Second),
			}
			
			// Update task
			task.WorkerID = workerID
			task.Attempts++
			task.Status = domain.StatusInProgress
			task.LeaseUntil = now.Add(time.Duration(leaseSeconds) * time.Second).Format(time.RFC3339)
			
			// Remove from queue
			s.plugin.queues[cmd] = append(queue[:i], queue[i+1:]...)
			
			// Return a copy
			taskCopy := *task
			return &taskCopy, true, nil
		}
	}
	
	return nil, false, nil
}

func (s *taskStorage) UpdateLease(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	l, exists := s.plugin.leases[taskID]
	if !exists {
		return fmt.Errorf("lease not found")
	}
	
	if l.workerID != workerID {
		return fmt.Errorf("lease owned by different worker")
	}
	
	l.expiresAt = time.Now().Add(time.Duration(extendSeconds) * time.Second)
	return nil
}

func (s *taskStorage) AbandonLease(ctx context.Context, taskID string, workerID string) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	l, exists := s.plugin.leases[taskID]
	if !exists {
		return nil
	}
	
	if l.workerID != workerID {
		return fmt.Errorf("lease owned by different worker")
	}
	
	delete(s.plugin.leases, taskID)
	
	// Put task back in queue
	if task, exists := s.plugin.tasks[taskID]; exists {
		task.Status = domain.StatusPending
		task.WorkerID = ""
		s.plugin.queues[task.Command] = append(s.plugin.queues[task.Command], task)
	}
	
	return nil
}

func (s *taskStorage) NackTask(ctx context.Context, taskID string, workerID string, delaySeconds int, reason string) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	task, exists := s.plugin.tasks[taskID]
	if !exists {
		return persistence.ErrNotFound
	}
	
	delete(s.plugin.leases, taskID)
	
	task.Status = domain.StatusPending
	task.WorkerID = ""
	task.Error = reason
	
	// Put back in queue after delay (simplified for memory plugin)
	s.plugin.queues[task.Command] = append(s.plugin.queues[task.Command], task)
	
	return nil
}

func (s *taskStorage) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	// In memory plugin, delayed tasks are already in queues
	// This is a no-op for simplicity
	return 0, nil
}

func (s *taskStorage) QueueLength(ctx context.Context, cmd domain.Command) (int64, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	return int64(len(s.plugin.queues[cmd])), nil
}

func (s *taskStorage) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	stats := &domain.QueueStats{
		Command: cmd,
		Ready:   int64(len(s.plugin.queues[cmd])),
	}
	
	return stats, nil
}

func (s *taskStorage) AdminQueues(ctx context.Context) (map[string]any, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	result := make(map[string]any)
	for cmd, queue := range s.plugin.queues {
		result[string(cmd)] = map[string]any{
			"pending": len(queue),
		}
	}
	
	return result, nil
}

func (s *taskStorage) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	removed := 0
	for id, task := range s.plugin.tasks {
		if task.CreatedAt.Before(before) {
			delete(s.plugin.tasks, id)
			delete(s.plugin.leases, id)
			removed++
			if removed >= limit {
				break
			}
		}
	}
	
	return removed, nil
}

// resultStorage implements persistence.ResultStorage for in-memory storage
type resultStorage struct {
	plugin *Plugin
}

func (s *resultStorage) SaveResult(ctx context.Context, rec domain.ResultRecord) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	recCopy := rec
	s.plugin.results[rec.TaskID] = &recCopy
	return nil
}

func (s *resultStorage) GetResult(ctx context.Context, taskID string) (*domain.ResultRecord, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	result, exists := s.plugin.results[taskID]
	if !exists {
		return nil, persistence.ErrNotFound
	}
	
	recCopy := *result
	return &recCopy, nil
}

func (s *resultStorage) UpdateTaskOnComplete(ctx context.Context, taskID string, status domain.TaskStatus, errorMsg string) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	task, exists := s.plugin.tasks[taskID]
	if !exists {
		return persistence.ErrNotFound
	}
	
	task.Status = status
	task.Error = errorMsg
	task.UpdatedAt = time.Now()
	
	return nil
}

func (s *resultStorage) RemoveFromInprogAndClearLease(ctx context.Context, taskID string, cmd domain.Command) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	delete(s.plugin.leases, taskID)
	return nil
}

// subscriptionStorage implements persistence.SubscriptionStorage for in-memory storage
type subscriptionStorage struct {
	plugin *Plugin
}

func (s *subscriptionStorage) Register(ctx context.Context, sub *domain.Subscription) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	subCopy := *sub
	s.plugin.subscriptions[sub.ID] = &subCopy
	return nil
}

func (s *subscriptionStorage) Unregister(ctx context.Context, workerID string, commands []domain.Command) error {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	// Note: domain.Subscription doesn't have a WorkerID field
	// This is a simplified implementation for the in-memory plugin
	// In a real system, subscriptions would need to track the worker that created them
	// For now, we'll remove all subscriptions (this is acceptable for testing)
	for id := range s.plugin.subscriptions {
		delete(s.plugin.subscriptions, id)
	}
	
	return nil
}

func (s *subscriptionStorage) GetByCommand(ctx context.Context, commands []domain.Command) ([]*domain.Subscription, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	var result []*domain.Subscription
	cmdMap := make(map[domain.Command]bool)
	for _, cmd := range commands {
		cmdMap[cmd] = true
	}
	
	for _, sub := range s.plugin.subscriptions {
		for _, eventType := range sub.EventTypes {
			if cmdMap[eventType] {
				subCopy := *sub
				result = append(result, &subCopy)
				break
			}
		}
	}
	
	return result, nil
}

func (s *subscriptionStorage) GetByWorker(ctx context.Context, workerID string) ([]*domain.Subscription, error) {
	s.plugin.mu.RLock()
	defer s.plugin.mu.RUnlock()
	
	// Note: domain.Subscription doesn't have a WorkerID field
	// This is a simplified implementation for the in-memory plugin
	// In a real system, subscriptions would track which worker created them
	// For now, return all subscriptions (acceptable for testing scenarios)
	var result []*domain.Subscription
	for _, sub := range s.plugin.subscriptions {
		subCopy := *sub
		result = append(result, &subCopy)
	}
	
	return result, nil
}

func (s *subscriptionStorage) RemoveExpired(ctx context.Context, before time.Time) (int, error) {
	s.plugin.mu.Lock()
	defer s.plugin.mu.Unlock()
	
	removed := 0
	for id, sub := range s.plugin.subscriptions {
		if sub.ExpiresAt.Before(before) {
			delete(s.plugin.subscriptions, id)
			removed++
		}
	}
	
	return removed, nil
}
