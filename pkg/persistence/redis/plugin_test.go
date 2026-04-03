package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
	"github.com/osvaldoandrade/codeq/pkg/persistence/persistencetest"
)

// setupRedisPlugin creates a Redis plugin backed by miniredis for testing.
func setupRedisPlugin(t *testing.T) (persistence.PluginPersistence, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("Failed to start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	cfg := persistence.PluginConfig{
		Config:             []byte(fmt.Sprintf(`{"addr":"%s"}`, mr.Addr())),
		Timezone:           time.UTC,
		BackoffPolicy:      "exp_full_jitter",
		BackoffBaseSeconds: 5,
		BackoffMaxSeconds:  900,
	}

	plugin, err := NewPlugin(cfg)
	if err != nil {
		t.Fatalf("Failed to create Redis plugin: %v", err)
	}
	t.Cleanup(func() { plugin.Close() })

	return plugin, mr
}

// TestRedisPluginContractTests runs the shared contract test suite against the
// Redis plugin to ensure it satisfies the same behavioral contract as all other
// persistence backends.
func TestRedisPluginContractTests(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	persistencetest.RunPluginContractTests(t, plugin)
}

// TestRedisPluginHealth verifies the Redis plugin health check communicates
// with the underlying Redis instance.
func TestRedisPluginHealth(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	ctx := context.Background()

	if err := plugin.Health(ctx); err != nil {
		t.Errorf("Health() error: %v", err)
	}
}

// TestRedisPluginRegistration verifies the Redis plugin self-registers
// via its init() function.
func TestRedisPluginRegistration(t *testing.T) {
	providers := persistence.ListProviders()
	found := false
	for _, p := range providers {
		if p == "redis" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Redis provider not registered; expected 'redis' in provider list")
	}
}

// TestRedisPluginNewPersistence verifies that the Redis plugin can be
// created through the registry's NewPersistence factory.
func TestRedisPluginNewPersistence(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("Failed to start miniredis: %v", err)
	}
	defer mr.Close()

	plugin, err := persistence.NewPersistence(
		persistence.ProviderConfig{
			Type:   "redis",
			Config: []byte(fmt.Sprintf(`{"addr":"%s"}`, mr.Addr())),
		},
		persistence.PluginConfig{
			Timezone:           time.UTC,
			BackoffPolicy:      "exp_full_jitter",
			BackoffBaseSeconds: 5,
			BackoffMaxSeconds:  900,
		},
	)
	if err != nil {
		t.Fatalf("NewPersistence() error: %v", err)
	}
	defer plugin.Close()

	if err := plugin.Health(context.Background()); err != nil {
		t.Fatalf("Health() error after registry creation: %v", err)
	}
}

// TestRedisPluginInvalidConfig verifies that the Redis plugin returns an error
// for invalid JSON configuration.
func TestRedisPluginInvalidConfig(t *testing.T) {
	_, err := NewPlugin(persistence.PluginConfig{
		Config: []byte("not-json"),
	})
	if err == nil {
		t.Error("NewPlugin() expected error for invalid JSON config, got nil")
	}
}

// TestRedisPluginTaskEnqueueClaim tests the full task lifecycle through the
// plugin interface: enqueue → claim → get → heartbeat.
// Note: The Redis backend generates a new UUID on enqueue, so Get uses the
// claimed task's actual ID.
func TestRedisPluginTaskEnqueueClaim(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	ctx := context.Background()
	ts := plugin.TaskStorage()

	task := &domain.Task{
		ID:          "redis-lifecycle-1",
		Command:     domain.CmdGenerateMaster,
		Payload:     `{"input":"data"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Enqueue
	if err := ts.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask() error: %v", err)
	}

	// Claim (Redis generates a new UUID, so we must claim to discover it)
	claimed, found, err := ts.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 100, "")
	if err != nil {
		t.Fatalf("ClaimTask() error: %v", err)
	}
	if !found {
		t.Fatal("ClaimTask() found = false, want true")
	}
	if claimed.WorkerID != "worker-1" {
		t.Errorf("ClaimTask() workerID = %v, want worker-1", claimed.WorkerID)
	}
	if claimed.Command != task.Command {
		t.Errorf("ClaimTask() command = %v, want %v", claimed.Command, task.Command)
	}
	if claimed.Payload != task.Payload {
		t.Errorf("ClaimTask() payload = %v, want %v", claimed.Payload, task.Payload)
	}

	// Get using the actual ID from the claimed task
	got, err := ts.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if got.Payload != task.Payload {
		t.Errorf("Get() payload = %v, want %v", got.Payload, task.Payload)
	}

	// Update lease (heartbeat)
	if err := ts.UpdateLease(ctx, claimed.ID, "worker-1", 120); err != nil {
		t.Fatalf("UpdateLease() error: %v", err)
	}
}

// TestRedisPluginTaskNack tests that a task can be nacked and returned to the queue.
func TestRedisPluginTaskNack(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	ctx := context.Background()
	ts := plugin.TaskStorage()

	task := &domain.Task{
		ID:          "redis-nack-1",
		Command:     domain.CmdGenerateCreative,
		Payload:     `{"nack":"test"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 5,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := ts.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask() error: %v", err)
	}

	// Claim first
	claimed, found, err := ts.ClaimTask(ctx, "worker-2", []domain.Command{domain.CmdGenerateCreative}, 60, 100, "")
	if err != nil {
		t.Fatalf("ClaimTask() error: %v", err)
	}
	if !found {
		t.Fatal("ClaimTask() found = false, want true")
	}

	// Nack with reason
	if err := ts.NackTask(ctx, claimed.ID, "worker-2", 0, "processing error"); err != nil {
		t.Fatalf("NackTask() error: %v", err)
	}
}

// TestRedisPluginResultStorage tests saving and retrieving results through
// the plugin interface.
func TestRedisPluginResultStorage(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	ctx := context.Background()
	ts := plugin.TaskStorage()
	rs := plugin.ResultStorage()

	// Must enqueue and claim to get the real task ID
	task := &domain.Task{
		ID:          "redis-result-1",
		Command:     domain.CmdGenerateMaster,
		Payload:     `{"result":"test"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := ts.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask() error: %v", err)
	}

	claimed, found, err := ts.ClaimTask(ctx, "worker-result", []domain.Command{domain.CmdGenerateMaster}, 60, 100, "")
	if err != nil {
		t.Fatalf("ClaimTask() error: %v", err)
	}
	if !found {
		t.Fatal("ClaimTask() found = false, want true")
	}

	// Save result using the actual task ID
	rec := domain.ResultRecord{
		TaskID:      claimed.ID,
		Status:      domain.StatusCompleted,
		Result:      map[string]any{"output": "success"},
		CompletedAt: time.Now(),
	}
	if err := rs.SaveResult(ctx, rec); err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	// Get result
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
}

// TestRedisPluginSubscriptionStorage tests subscription register and query
// through the plugin interface.
func TestRedisPluginSubscriptionStorage(t *testing.T) {
	plugin, _ := setupRedisPlugin(t)
	ctx := context.Background()
	ss := plugin.SubscriptionStorage()

	sub := &domain.Subscription{
		ID:          "redis-sub-1",
		CallbackURL: "http://example.com/hook",
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

	// Verify Unregister does not error
	if err := ss.Unregister(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}); err != nil {
		t.Fatalf("Unregister() error: %v", err)
	}
}

// TestRedisPluginDataFormatCompatibility verifies that data written through
// the plugin uses the same Redis key formats as the direct repository
// implementation, ensuring existing data remains readable.
func TestRedisPluginDataFormatCompatibility(t *testing.T) {
	plugin, mr := setupRedisPlugin(t)
	ctx := context.Background()
	ts := plugin.TaskStorage()

	task := &domain.Task{
		ID:          "compat-task-1",
		Command:     domain.CmdGenerateMaster,
		Payload:     `{"compat":"test"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := ts.EnqueueTask(ctx, task); err != nil {
		t.Fatalf("EnqueueTask() error: %v", err)
	}

	// Verify the task is stored in the expected Redis HASH key
	if !mr.Exists("codeq:tasks") {
		t.Error("Expected 'codeq:tasks' key to exist in Redis")
	}

	// Verify at least one task field exists in the hash (the ID is generated)
	fields, err := mr.HKeys("codeq:tasks")
	if err != nil {
		t.Fatalf("HKeys(codeq:tasks) error: %v", err)
	}
	if len(fields) == 0 {
		t.Error("Expected at least one field in codeq:tasks hash")
	}

	// Verify the task data is readable from the hash
	taskID := fields[0]
	val := mr.HGet("codeq:tasks", taskID)
	if val == "" {
		t.Error("Expected task data in codeq:tasks hash, got empty string")
	}

	// Verify the TTL tracking key exists
	if !mr.Exists("codeq:tasks:ttl") {
		t.Error("Expected 'codeq:tasks:ttl' key to exist in Redis")
	}

	// Verify the pending queue key format uses lowercase command:
	// codeq:q:generate_master:pending:5
	pendingKey := "codeq:q:generate_master:pending:5"
	if !mr.Exists(pendingKey) {
		t.Errorf("Expected pending queue key '%s' to exist in Redis", pendingKey)
	}
}

// TestRedisPluginConfigMigration verifies that both the old-style
// (direct addr/password) and new-style (via ProviderConfig) configurations
// produce a working plugin, validating the migration path.
func TestRedisPluginConfigMigration(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("Failed to start miniredis: %v", err)
	}
	defer mr.Close()

	t.Run("DirectPluginConfig", func(t *testing.T) {
		plugin, err := NewPlugin(persistence.PluginConfig{
			Config:             []byte(fmt.Sprintf(`{"addr":"%s"}`, mr.Addr())),
			Timezone:           time.UTC,
			BackoffPolicy:      "exp_full_jitter",
			BackoffBaseSeconds: 5,
			BackoffMaxSeconds:  900,
		})
		if err != nil {
			t.Fatalf("NewPlugin() error: %v", err)
		}
		defer plugin.Close()

		if err := plugin.Health(context.Background()); err != nil {
			t.Errorf("Health() error: %v", err)
		}
	})

	t.Run("ViaProviderRegistry", func(t *testing.T) {
		plugin, err := persistence.NewPersistence(
			persistence.ProviderConfig{
				Type:   "redis",
				Config: []byte(fmt.Sprintf(`{"addr":"%s"}`, mr.Addr())),
			},
			persistence.PluginConfig{
				Timezone:           time.UTC,
				BackoffPolicy:      "exp_full_jitter",
				BackoffBaseSeconds: 5,
				BackoffMaxSeconds:  900,
			},
		)
		if err != nil {
			t.Fatalf("NewPersistence() error: %v", err)
		}
		defer plugin.Close()

		if err := plugin.Health(context.Background()); err != nil {
			t.Errorf("Health() error: %v", err)
		}
	})

	t.Run("WithPasswordConfig", func(t *testing.T) {
		plugin, err := NewPlugin(persistence.PluginConfig{
			Config:   []byte(fmt.Sprintf(`{"addr":"%s","password":""}`, mr.Addr())),
			Timezone: time.UTC,
		})
		if err != nil {
			t.Fatalf("NewPlugin() error: %v", err)
		}
		defer plugin.Close()

		if err := plugin.Health(context.Background()); err != nil {
			t.Errorf("Health() error: %v", err)
		}
	})
}
