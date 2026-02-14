package services

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func setupSchedulerTest(t *testing.T) (context.Context, *miniredis.Miniredis, *redis.Client, repository.TaskRepository, SchedulerService) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	
	repo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	
	// Create a mock subscription repository for the notifier
	mockSubRepo := &mockSubscriptionRepo{}
	notifier := NewNotifierService(mockSubRepo, nil, "test-secret", 5)
	
	now := func() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) }
	svc := NewSchedulerService(repo, notifier, time.UTC, now, 60, 50, 5, "exp_full_jitter", 5, 900)
	
	return context.Background(), mr, rdb, repo, svc
}

// Mock subscription repository
type mockSubscriptionRepo struct{}

func (m *mockSubscriptionRepo) Create(ctx context.Context, sub domain.Subscription, ttlSeconds int) (*domain.Subscription, error) {
	return nil, nil
}

func (m *mockSubscriptionRepo) Get(ctx context.Context, id string) (*domain.Subscription, error) {
	return nil, nil
}

func (m *mockSubscriptionRepo) Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error) {
	return nil, nil
}

func (m *mockSubscriptionRepo) ListActive(ctx context.Context, cmd domain.Command, now time.Time) ([]domain.Subscription, error) {
	return nil, nil
}

func (m *mockSubscriptionRepo) AllowNotify(ctx context.Context, id string, minIntervalSeconds int) (bool, error) {
	return true, nil
}

func (m *mockSubscriptionRepo) NextGroupIndex(ctx context.Context, cmd domain.Command, groupID string, mod int) (int, error) {
	return 0, nil
}

func (m *mockSubscriptionRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	return 0, nil
}

func TestCreateTaskSuccess(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "https://example.com/webhook", 3, "", time.Time{}, 0)
	
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if task == nil {
		t.Fatal("Expected task to be non-nil")
	}
	if task.Command != domain.CmdGenerateMaster {
		t.Errorf("Expected command %s, got %s", domain.CmdGenerateMaster, task.Command)
	}
	if task.MaxAttempts != 3 {
		t.Errorf("Expected maxAttempts 3, got %d", task.MaxAttempts)
	}
}

func TestCreateTaskEmptyCommand(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	_, err := svc.CreateTask(ctx, "", `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	
	if err == nil {
		t.Fatal("Expected error for empty command")
	}
	if err.Error() != "invalid command" {
		t.Errorf("Expected 'invalid command', got %v", err)
	}
}

func TestCreateTaskInvalidWebhook(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	tests := []struct {
		name    string
		webhook string
	}{
		{"invalid url", "not-a-url"},
		{"ftp scheme", "ftp://example.com"},
		{"no host", "http://"},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, tt.webhook, 3, "", time.Time{}, 0)
			if err == nil {
				t.Fatal("Expected error for invalid webhook")
			}
			if err.Error() != "invalid webhook url" {
				t.Errorf("Expected 'invalid webhook url', got %v", err)
			}
		})
	}
}

func TestCreateTaskDefaultMaxAttempts(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 0, "", time.Time{}, 0)
	
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if task.MaxAttempts != 5 {
		t.Errorf("Expected default maxAttempts 5, got %d", task.MaxAttempts)
	}
}

func TestCreateTaskWithDelay(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 60)
	
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if task == nil {
		t.Fatal("Expected task to be non-nil")
	}
	// Task should be created with delayed visibility
}

func TestCreateTaskWithRunAt(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	runAt := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", runAt, 0)
	
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if task == nil {
		t.Fatal("Expected task to be non-nil")
	}
}

func TestCreateTaskIdempotent(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	idempotencyKey := "test-key-123"
	
	task1, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, idempotencyKey, time.Time{}, 0)
	if err != nil {
		t.Fatalf("CreateTask 1 failed: %v", err)
	}
	
	task2, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, idempotencyKey, time.Time{}, 0)
	if err != nil {
		t.Fatalf("CreateTask 2 failed: %v", err)
	}
	
	if task1.ID != task2.ID {
		t.Errorf("Expected same task ID for idempotency, got %s and %s", task1.ID, task2.ID)
	}
}

func TestClaimTaskSuccess(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create a task first
	_, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	
	// Claim the task
	task, ok, err := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}
	if !ok {
		t.Fatal("Expected claim to succeed")
	}
	if task == nil {
		t.Fatal("Expected task to be non-nil")
	}
	if task.WorkerID != "worker-1" {
		t.Errorf("Expected worker-1, got %s", task.WorkerID)
	}
}

func TestClaimTaskEmptyWorkerID(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	_, _, err := svc.ClaimTask(ctx, "", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	
	if err == nil {
		t.Fatal("Expected error for empty workerID")
	}
	if err.Error() != "workerId is required" {
		t.Errorf("Expected 'workerId is required', got %v", err)
	}
}

func TestClaimTaskDefaultCommands(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create tasks for both default commands
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	
	// Claim with empty commands (should default)
	task, ok, err := svc.ClaimTask(ctx, "worker-1", []domain.Command{}, 60, 0)
	
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}
	if ok && task == nil {
		t.Fatal("Expected task to be non-nil when ok is true")
	}
}

func TestClaimTaskWithWait(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Claim with wait but no tasks available - should timeout quickly
	start := time.Now()
	_, ok, err := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 1)
	duration := time.Since(start)
	
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}
	if ok {
		t.Fatal("Expected claim to fail (no tasks)")
	}
	if duration < 500*time.Millisecond {
		t.Errorf("Expected to wait at least 500ms, waited %v", duration)
	}
}

func TestHeartbeatSuccess(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Heartbeat
	err := svc.Heartbeat(ctx, task.ID, "worker-1", 30)
	if err != nil {
		t.Errorf("Heartbeat failed: %v", err)
	}
}

func TestHeartbeatDefaultExtend(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Heartbeat with 0 extend (should use default)
	err := svc.Heartbeat(ctx, task.ID, "worker-1", 0)
	if err != nil {
		t.Errorf("Heartbeat failed: %v", err)
	}
}

func TestAbandonTask(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Abandon
	err := svc.Abandon(ctx, task.ID, "worker-1")
	if err != nil {
		t.Errorf("Abandon failed: %v", err)
	}
}

func TestNackTaskSuccess(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Nack
	delay, dlq, err := svc.NackTask(ctx, task.ID, "worker-1", 0, "test error")
	
	if err != nil {
		t.Fatalf("NackTask failed: %v", err)
	}
	if dlq {
		t.Error("Expected task not to be in DLQ on first nack")
	}
	if delay < 0 {
		t.Errorf("Expected non-negative delay, got %d", delay)
	}
}

func TestNackTaskEmptyWorkerID(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	_, _, err := svc.NackTask(ctx, "task-123", "", 0, "error")
	
	if err == nil {
		t.Fatal("Expected error for empty workerID")
	}
	if err.Error() != "workerId is required" {
		t.Errorf("Expected 'workerId is required', got %v", err)
	}
}

func TestNackTaskNotFound(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	_, _, err := svc.NackTask(ctx, "nonexistent-task", "worker-1", 0, "error")
	
	if err == nil {
		t.Fatal("Expected error for nonexistent task")
	}
}

func TestNackTaskWithExplicitDelay(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Nack with explicit delay
	delay, _, err := svc.NackTask(ctx, task.ID, "worker-1", 30, "test error")
	
	if err != nil {
		t.Fatalf("NackTask failed: %v", err)
	}
	if delay != 30 {
		t.Errorf("Expected delay 30, got %d", delay)
	}
}

func TestNackTaskDelayExceedsMax(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create and claim a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	task, ok, _ := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0)
	if !ok {
		t.Fatal("Failed to claim task")
	}
	
	// Nack with delay exceeding max (900 seconds)
	delay, _, err := svc.NackTask(ctx, task.ID, "worker-1", 10000, "test error")
	
	if err != nil {
		t.Fatalf("NackTask failed: %v", err)
	}
	if delay > 900 {
		t.Errorf("Expected delay capped at 900, got %d", delay)
	}
}

func TestGetTask(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create a task
	created, _ := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	
	// Get the task
	task, err := svc.GetTask(ctx, created.ID)
	
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if task.ID != created.ID {
		t.Errorf("Expected task ID %s, got %s", created.ID, task.ID)
	}
}

func TestAdminQueues(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	queues, err := svc.AdminQueues(ctx)
	
	if err != nil {
		t.Fatalf("AdminQueues failed: %v", err)
	}
	if queues == nil {
		t.Fatal("Expected queues to be non-nil")
	}
}

func TestQueueStats(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create a task
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	
	// Get stats
	stats, err := svc.QueueStats(ctx, domain.CmdGenerateMaster)
	
	if err != nil {
		t.Fatalf("QueueStats failed: %v", err)
	}
	if stats == nil {
		t.Fatal("Expected stats to be non-nil")
	}
}

func TestCleanupExpired(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Create some tasks
	_, _ = svc.CreateTask(ctx, domain.CmdGenerateMaster, `{"key":"value"}`, 5, "", 3, "", time.Time{}, 0)
	
	// Cleanup with future date
	deleted, err := svc.CleanupExpired(ctx, 10, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	
	if err != nil {
		t.Fatalf("CleanupExpired failed: %v", err)
	}
	if deleted < 0 {
		t.Errorf("Expected non-negative deleted count, got %d", deleted)
	}
}

func TestCleanupExpiredDefaultLimit(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Cleanup with zero limit (should use default 1000)
	deleted, err := svc.CleanupExpired(ctx, 0, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	
	if err != nil {
		t.Fatalf("CleanupExpired failed: %v", err)
	}
	if deleted < 0 {
		t.Errorf("Expected non-negative deleted count, got %d", deleted)
	}
}

func TestCleanupExpiredZeroBefore(t *testing.T) {
	ctx, _, _, _, svc := setupSchedulerTest(t)
	
	// Cleanup with zero time (should use current time from now())
	deleted, err := svc.CleanupExpired(ctx, 10, time.Time{})
	
	if err != nil {
		t.Fatalf("CleanupExpired failed: %v", err)
	}
	if deleted < 0 {
		t.Errorf("Expected non-negative deleted count, got %d", deleted)
	}
}

func TestNewSchedulerServiceDefaults(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	
	repo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	mockSubRepo := &mockSubscriptionRepo{}
	notifier := NewNotifierService(mockSubRepo, nil, "test-secret", 5)
	
	tests := []struct {
		name              string
		maxAttemptsInput  int
		backoffBaseInput  int
		backoffMaxInput   int
		backoffPolicyInput string
	}{
		{"zero maxAttempts", 0, 5, 900, "exp_full_jitter"},
		{"negative maxAttempts", -1, 5, 900, "exp_full_jitter"},
		{"zero backoff base", 5, 0, 900, "exp_full_jitter"},
		{"zero backoff max", 5, 5, 0, "exp_full_jitter"},
		{"empty policy", 5, 5, 900, ""},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := func() time.Time { return time.Now() }
			svc := NewSchedulerService(repo, notifier, time.UTC, now, 60, 50, tt.maxAttemptsInput, tt.backoffPolicyInput, tt.backoffBaseInput, tt.backoffMaxInput)
			if svc == nil {
				t.Fatal("Expected service to be non-nil")
			}
		})
	}
}
