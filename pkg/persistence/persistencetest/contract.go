// Package persistencetest provides shared contract tests that any persistence
// plugin implementation must pass. These tests verify interface compliance and
// behavioral consistency across all backends.
package persistencetest

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
)

// RunPluginContractTests runs the full contract test suite against a plugin.
// Every persistence plugin (Redis, Memory, future implementations) must pass
// these tests to ensure behavioral consistency.
func RunPluginContractTests(t *testing.T, plugin persistence.PluginPersistence) {
	t.Helper()

	t.Run("Health", func(t *testing.T) { testHealth(t, plugin) })
	t.Run("TaskStorage", func(t *testing.T) { testTaskStorage(t, plugin) })
	t.Run("ResultStorage", func(t *testing.T) { testResultStorage(t, plugin) })
	t.Run("SubscriptionStorage", func(t *testing.T) { testSubscriptionStorage(t, plugin) })
}

func testHealth(t *testing.T, plugin persistence.PluginPersistence) {
	t.Helper()
	ctx := context.Background()
	if err := plugin.Health(ctx); err != nil {
		t.Errorf("Health() returned error: %v", err)
	}
}

func testTaskStorage(t *testing.T, plugin persistence.PluginPersistence) {
	t.Helper()
	ts := plugin.TaskStorage()
	if ts == nil {
		t.Fatal("TaskStorage() returned nil")
	}

	t.Run("EnqueueAndClaim", func(t *testing.T) {
		ctx := context.Background()
		cmd := domain.Command("CONTRACT_ENQUEUE_CLAIM")
		task := &domain.Task{
			ID:          "contract-task-1",
			Command:     cmd,
			Payload:     `{"key":"value"}`,
			Priority:    5,
			Status:      domain.StatusPending,
			MaxAttempts: 3,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		if err := ts.EnqueueTask(ctx, task); err != nil {
			t.Fatalf("EnqueueTask() error: %v", err)
		}

		// Claim the task; some backends may generate a new ID
		claimed, found, err := ts.ClaimTask(ctx, "contract-worker-1", []domain.Command{cmd}, 60, 100, "")
		if err != nil {
			t.Fatalf("ClaimTask() error: %v", err)
		}
		if !found {
			t.Fatal("ClaimTask() found = false, want true")
		}
		if claimed.Command != cmd {
			t.Errorf("ClaimTask() command = %v, want %v", claimed.Command, cmd)
		}
		if claimed.Payload != task.Payload {
			t.Errorf("ClaimTask() payload = %v, want %v", claimed.Payload, task.Payload)
		}

		// Get using the claimed task's actual ID
		got, err := ts.Get(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("Get() error: %v", err)
		}
		if got.Payload != task.Payload {
			t.Errorf("Get() payload = %v, want %v", got.Payload, task.Payload)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		ctx := context.Background()
		_, err := ts.Get(ctx, "nonexistent-task-id")
		if err == nil {
			t.Error("Get() expected error for nonexistent task, got nil")
		}
	})

	t.Run("ClaimTaskEmptyQueue", func(t *testing.T) {
		ctx := context.Background()
		_, found, err := ts.ClaimTask(ctx, "contract-worker-2", []domain.Command{"NONEXISTENT_CMD"}, 60, 100, "")
		if err != nil {
			t.Fatalf("ClaimTask() error: %v", err)
		}
		if found {
			t.Error("ClaimTask() found = true on empty queue, want false")
		}
	})

	t.Run("QueueLength", func(t *testing.T) {
		ctx := context.Background()
		cmd := domain.Command("CONTRACT_QUEUE_LEN")
		task := &domain.Task{
			ID:          "contract-qlen-1",
			Command:     cmd,
			Payload:     `{}`,
			Priority:    5,
			Status:      domain.StatusPending,
			MaxAttempts: 3,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		if err := ts.EnqueueTask(ctx, task); err != nil {
			t.Fatalf("EnqueueTask() error: %v", err)
		}

		length, err := ts.QueueLength(ctx, cmd)
		if err != nil {
			t.Fatalf("QueueLength() error: %v", err)
		}
		if length < 1 {
			t.Errorf("QueueLength() = %d, want >= 1", length)
		}
	})

	t.Run("QueueStats", func(t *testing.T) {
		ctx := context.Background()
		cmd := domain.Command("CONTRACT_STATS")
		task := &domain.Task{
			ID:          "contract-stats-1",
			Command:     cmd,
			Payload:     `{}`,
			Priority:    5,
			Status:      domain.StatusPending,
			MaxAttempts: 3,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		if err := ts.EnqueueTask(ctx, task); err != nil {
			t.Fatalf("EnqueueTask() error: %v", err)
		}

		stats, err := ts.QueueStats(ctx, cmd)
		if err != nil {
			t.Fatalf("QueueStats() error: %v", err)
		}
		if stats == nil {
			t.Fatal("QueueStats() returned nil")
		}
		if stats.Ready < 1 {
			t.Errorf("QueueStats().Ready = %d, want >= 1", stats.Ready)
		}
	})

	t.Run("MoveDueDelayed", func(t *testing.T) {
		ctx := context.Background()
		cmd := domain.Command("CONTRACT_DELAYED")
		_, err := ts.MoveDueDelayed(ctx, cmd, 10)
		if err != nil {
			t.Fatalf("MoveDueDelayed() error: %v", err)
		}
	})

	t.Run("AdminQueues", func(t *testing.T) {
		ctx := context.Background()
		queues, err := ts.AdminQueues(ctx)
		if err != nil {
			t.Fatalf("AdminQueues() error: %v", err)
		}
		if queues == nil {
			t.Error("AdminQueues() returned nil")
		}
	})
}

func testResultStorage(t *testing.T, plugin persistence.PluginPersistence) {
	t.Helper()
	rs := plugin.ResultStorage()
	if rs == nil {
		t.Fatal("ResultStorage() returned nil")
	}

	ts := plugin.TaskStorage()
	ctx := context.Background()

	// Enqueue and claim a task to get a valid task ID in storage
	cmd := domain.Command("CONTRACT_RESULT")
	task := &domain.Task{
		ID:          "contract-result-task-1",
		Command:     cmd,
		Payload:     `{"result":"test"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := ts.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask() setup error: %v", err)
	}

	// Claim to get the real task ID (some backends generate new IDs)
	claimed, found, err := ts.ClaimTask(ctx, "contract-result-worker", []domain.Command{cmd}, 60, 100, "")
	if err != nil {
		t.Fatalf("ClaimTask() setup error: %v", err)
	}
	if !found {
		t.Fatal("ClaimTask() setup: found = false, want true")
	}

	t.Run("SaveAndGetResult", func(t *testing.T) {
		rec := domain.ResultRecord{
			TaskID:      claimed.ID,
			Status:      domain.StatusCompleted,
			Result:      map[string]any{"output": "done"},
			CompletedAt: time.Now(),
		}

		if err := rs.SaveResult(ctx, rec); err != nil {
			t.Fatalf("SaveResult() error: %v", err)
		}

		got, err := rs.GetResult(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("GetResult() error: %v", err)
		}
		if got.TaskID != claimed.ID {
			t.Errorf("GetResult() TaskID = %v, want %v", got.TaskID, claimed.ID)
		}
		if got.Status != domain.StatusCompleted {
			t.Errorf("GetResult() Status = %v, want %v", got.Status, domain.StatusCompleted)
		}
	})

	t.Run("GetResultNotFound", func(t *testing.T) {
		_, err := rs.GetResult(ctx, "nonexistent-result-id")
		if err == nil {
			t.Error("GetResult() expected error for nonexistent result, got nil")
		}
	})

	t.Run("UpdateTaskOnComplete", func(t *testing.T) {
		err := rs.UpdateTaskOnComplete(ctx, claimed.ID, domain.StatusCompleted, "")
		if err != nil {
			t.Fatalf("UpdateTaskOnComplete() error: %v", err)
		}
	})

	t.Run("RemoveFromInprogAndClearLease", func(t *testing.T) {
		err := rs.RemoveFromInprogAndClearLease(ctx, claimed.ID, cmd)
		if err != nil {
			t.Fatalf("RemoveFromInprogAndClearLease() error: %v", err)
		}
	})
}

func testSubscriptionStorage(t *testing.T, plugin persistence.PluginPersistence) {
	t.Helper()
	ss := plugin.SubscriptionStorage()
	if ss == nil {
		t.Fatal("SubscriptionStorage() returned nil")
	}

	ctx := context.Background()

	t.Run("RegisterAndGetByCommand", func(t *testing.T) {
		sub := &domain.Subscription{
			ID:          "contract-sub-1",
			CallbackURL: "http://example.com/webhook",
			EventTypes:  []domain.Command{domain.CmdGenerateMaster},
			ExpiresAt:   time.Now().Add(1 * time.Hour),
			CreatedAt:   time.Now(),
		}

		if err := ss.Register(ctx, sub); err != nil {
			t.Fatalf("Register() error: %v", err)
		}

		subs, err := ss.GetByCommand(ctx, []domain.Command{domain.CmdGenerateMaster})
		if err != nil {
			t.Fatalf("GetByCommand() error: %v", err)
		}
		if len(subs) < 1 {
			t.Fatal("GetByCommand() returned 0 subscriptions, want >= 1")
		}
	})

	t.Run("RemoveExpired", func(t *testing.T) {
		// RemoveExpired should not error; the count varies by backend
		_, err := ss.RemoveExpired(ctx, time.Now())
		if err != nil {
			t.Fatalf("RemoveExpired() error: %v", err)
		}
	})
}
