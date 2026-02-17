package memory

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
)

func TestMemoryPlugin(t *testing.T) {
	// Create plugin
	cfg := persistence.PluginConfig{
		Config:             []byte("{}"),
		Timezone:           time.UTC,
		BackoffPolicy:      "exp_full_jitter",
		BackoffBaseSeconds: 5,
		BackoffMaxSeconds:  900,
	}

	plugin, err := NewPlugin(cfg)
	if err != nil {
		t.Fatalf("Failed to create plugin: %v", err)
	}
	defer plugin.Close()

	// Test health
	ctx := context.Background()
	if err := plugin.Health(ctx); err != nil {
		t.Errorf("Health check failed: %v", err)
	}

	// Test task storage
	taskStorage := plugin.TaskStorage()
	if taskStorage == nil {
		t.Fatal("TaskStorage returned nil")
	}

	// Create and enqueue a task
	task := &domain.Task{
		ID:          "test-task-1",
		Command:     domain.CmdGenerateMaster,
		Payload:     `{"test":"data"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 5,
		TenantID:    "tenant-1",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := taskStorage.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}

	// Get the task
	retrieved, err := taskStorage.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if retrieved.ID != task.ID {
		t.Errorf("Retrieved task ID mismatch: got %s, want %s", retrieved.ID, task.ID)
	}

	// Claim the task
	claimed, found, err := taskStorage.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 100, "tenant-1")
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}
	if !found {
		t.Fatal("Expected to find claimable task")
	}
	if claimed.ID != task.ID {
		t.Errorf("Claimed task ID mismatch: got %s, want %s", claimed.ID, task.ID)
	}
	if claimed.WorkerID != "worker-1" {
		t.Errorf("Claimed task WorkerID mismatch: got %s, want worker-1", claimed.WorkerID)
	}

	// Test result storage
	resultStorage := plugin.ResultStorage()
	if resultStorage == nil {
		t.Fatal("ResultStorage returned nil")
	}

	// Save a result
	result := domain.ResultRecord{
		TaskID:      task.ID,
		Status:      domain.StatusCompleted,
		Result:      map[string]any{"output": "success"},
		CompletedAt: time.Now(),
	}

	if err := resultStorage.SaveResult(ctx, result); err != nil {
		t.Fatalf("SaveResult failed: %v", err)
	}

	// Get the result
	retrievedResult, err := resultStorage.GetResult(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetResult failed: %v", err)
	}
	if retrievedResult.TaskID != task.ID {
		t.Errorf("Retrieved result TaskID mismatch: got %s, want %s", retrievedResult.TaskID, task.ID)
	}

	// Test subscription storage
	subStorage := plugin.SubscriptionStorage()
	if subStorage == nil {
		t.Fatal("SubscriptionStorage returned nil")
	}

	// Register a subscription
	sub := &domain.Subscription{
		ID:          "sub-1",
		CallbackURL: "http://example.com/webhook",
		EventTypes:  []domain.Command{domain.CmdGenerateMaster},
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		CreatedAt:   time.Now(),
	}

	if err := subStorage.Register(ctx, sub); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Get subscriptions by command
	subs, err := subStorage.GetByCommand(ctx, []domain.Command{domain.CmdGenerateMaster})
	if err != nil {
		t.Fatalf("GetByCommand failed: %v", err)
	}
	if len(subs) != 1 {
		t.Errorf("Expected 1 subscription, got %d", len(subs))
	}
	if subs[0].ID != sub.ID {
		t.Errorf("Subscription ID mismatch: got %s, want %s", subs[0].ID, sub.ID)
	}
}

func TestMemoryPluginNotFound(t *testing.T) {
	cfg := persistence.PluginConfig{
		Config:             []byte("{}"),
		Timezone:           time.UTC,
		BackoffPolicy:      "exp_full_jitter",
		BackoffBaseSeconds: 5,
		BackoffMaxSeconds:  900,
	}

	plugin, err := NewPlugin(cfg)
	if err != nil {
		t.Fatalf("Failed to create plugin: %v", err)
	}
	defer plugin.Close()

	ctx := context.Background()
	taskStorage := plugin.TaskStorage()

	// Try to get non-existent task
	_, err = taskStorage.Get(ctx, "non-existent")
	if err != persistence.ErrNotFound {
		t.Errorf("Expected ErrNotFound, got %v", err)
	}
}
