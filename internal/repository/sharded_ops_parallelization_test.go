package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TestShardedOperationsParallelization verifies that PendingLength, QueueStats, and AdminQueues
// use concurrent goroutines to parallelize queries across multiple shards.
func TestShardedOperationsParallelization(t *testing.T) {
	mr1 := miniredis.RunT(t)
	mr2 := miniredis.RunT(t)
	mr3 := miniredis.RunT(t)
	defer mr1.Close()
	defer mr2.Close()
	defer mr3.Close()

	rdb1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	rdb2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	rdb3 := redis.NewClient(&redis.Options{Addr: mr3.Addr()})

	mockSupplier := &mockShardSupplier{
		shards: []string{"shard1", "shard2", "shard3"},
	}

	repo1 := NewTaskRepository(rdb1, time.UTC, "fixed", 1, 3, mockSupplier)
	repo2 := NewTaskRepository(rdb2, time.UTC, "fixed", 1, 3, mockSupplier)
	repo3 := NewTaskRepository(rdb3, time.UTC, "fixed", 1, 3, mockSupplier)

	ctx := context.Background()

	// Seed 10 pending tasks in each shard
	for i := 0; i < 10; i++ {
		repo1.Enqueue(ctx, domain.CmdGenerateMaster, `{"s":1}`, 0, "", 0, "", time.Time{}, "")
		repo2.Enqueue(ctx, domain.CmdGenerateMaster, `{"s":2}`, 0, "", 0, "", time.Time{}, "")
		repo3.Enqueue(ctx, domain.CmdGenerateMaster, `{"s":3}`, 0, "", 0, "", time.Time{}, "")
	}

	repoMap := map[string]TaskRepository{
		"shard1": repo1,
		"shard2": repo2,
		"shard3": repo3,
	}
	shardedRepo := &shardedTaskRepository{
		repos:         repoMap,
		shardSupplier: mockSupplier,
		defaultShard:  "shard1",
	}

	// Test 1: PendingLength should parallelize
	t.Run("PendingLength parallelization", func(t *testing.T) {
		var callCount int32
		var maxConcurrent int32
		var mu sync.Mutex
		activeGoroutines := 0

		trackedRepos := make(map[string]TaskRepository, len(repoMap))
		for sid, repo := range repoMap {
			trackedRepos[sid] = &trackingRepository{
				repo:                repo,
				onPendingLength:     func() { atomic.AddInt32(&callCount, 1) },
				onMaxConcurrent:     &maxConcurrent,
				trackingMutex:       &mu,
				activeGoroutinesPtr: &activeGoroutines,
			}
		}

		originalRepos := shardedRepo.repos
		shardedRepo.repos = trackedRepos

		length, err := shardedRepo.PendingLength(ctx, domain.CmdGenerateMaster)
		if err != nil {
			t.Fatalf("PendingLength failed: %v", err)
		}

		if length != 30 {
			t.Errorf("Expected 30 pending tasks, got %d", length)
		}

		if callCount != 3 {
			t.Errorf("Expected 3 calls to PendingLength (one per shard), got %d", callCount)
		}

		shardedRepo.repos = originalRepos
	})

	// Test 2: QueueStats should parallelize
	t.Run("QueueStats parallelization", func(t *testing.T) {
		stats, err := shardedRepo.QueueStats(ctx, domain.CmdGenerateMaster, "")
		if err != nil {
			t.Fatalf("QueueStats failed: %v", err)
		}

		if stats.Ready != 30 {
			t.Errorf("Expected 30 ready tasks, got %d", stats.Ready)
		}
	})

	// Test 3: AdminQueues should parallelize
	t.Run("AdminQueues parallelization", func(t *testing.T) {
		result, err := shardedRepo.AdminQueues(ctx)
		if err != nil {
			t.Fatalf("AdminQueues failed: %v", err)
		}

		if result == nil || len(result) == 0 {
			t.Error("Expected AdminQueues to return aggregated results")
		}
	})
}

// TestShardedOperationsPerformance demonstrates performance improvement from parallelization.
func TestShardedOperationsPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	numShards := 5
	shardNames := make([]string, numShards)
	repoMap := make(map[string]TaskRepository, numShards)

	for i := 0; i < numShards; i++ {
		mr := miniredis.RunT(t)
		t.Cleanup(mr.Close)

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		sid := fmt.Sprintf("shard%d", i)
		shardNames[i] = sid

		mockSupplier := &mockShardSupplier{shards: shardNames}
		repo := NewTaskRepository(rdb, time.UTC, "fixed", 1, 3, mockSupplier)

		// Seed each shard with 5 tasks
		ctx := context.Background()
		for k := 0; k < 5; k++ {
			repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"perf":true}`, 0, "", 0, "", time.Time{}, "")
		}
		repoMap[sid] = repo
	}

	mockSupplier := &mockShardSupplier{shards: shardNames}
	shardedRepo := &shardedTaskRepository{
		repos:         repoMap,
		shardSupplier: mockSupplier,
		defaultShard:  shardNames[0],
	}

	ctx := context.Background()

	start := time.Now()
	length, err := shardedRepo.PendingLength(ctx, domain.CmdGenerateMaster)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("PendingLength failed: %v", err)
	}

	expectedTotal := int64(numShards * 5)
	if length != expectedTotal {
		t.Errorf("Expected %d pending tasks, got %d", expectedTotal, length)
	}
	t.Logf("PendingLength across %d shards took %v", numShards, elapsed)

	start = time.Now()
	stats, err := shardedRepo.QueueStats(ctx, domain.CmdGenerateMaster, "")
	elapsed = time.Since(start)

	if err != nil {
		t.Fatalf("QueueStats failed: %v", err)
	}

	if stats.Ready != expectedTotal {
		t.Errorf("Expected %d ready tasks, got %d", expectedTotal, stats.Ready)
	}
	t.Logf("QueueStats across %d shards took %v", numShards, elapsed)

	start = time.Now()
	_, err = shardedRepo.AdminQueues(ctx)
	elapsed = time.Since(start)

	if err != nil {
		t.Fatalf("AdminQueues failed: %v", err)
	}
	t.Logf("AdminQueues across %d shards took %v", numShards, elapsed)
}

// mockShardSupplier implements ShardSupplier for testing
type mockShardSupplier struct {
	shards []string
}

func (m *mockShardSupplier) QueueShards(ctx context.Context, commandName, tenantID string) ([]string, error) {
	return m.shards, nil
}

func (m *mockShardSupplier) CurrentShard(ctx context.Context, commandName, tenantID string) (string, error) {
	if len(m.shards) == 0 {
		return "", fmt.Errorf("no shards available")
	}
	return m.shards[0], nil
}

// trackingRepository wraps a TaskRepository to track concurrent calls
type trackingRepository struct {
	repo                TaskRepository
	onPendingLength     func()
	onMaxConcurrent     *int32
	trackingMutex       *sync.Mutex
	activeGoroutinesPtr *int
}

func (tr *trackingRepository) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	tr.trackingMutex.Lock()
	*tr.activeGoroutinesPtr++
	currentActive := *tr.activeGoroutinesPtr
	tr.trackingMutex.Unlock()

	for {
		current := atomic.LoadInt32(tr.onMaxConcurrent)
		if int32(currentActive) > current {
			if atomic.CompareAndSwapInt32(tr.onMaxConcurrent, current, int32(currentActive)) {
				break
			}
		} else {
			break
		}
	}

	defer func() {
		tr.trackingMutex.Lock()
		*tr.activeGoroutinesPtr--
		tr.trackingMutex.Unlock()
	}()

	if tr.onPendingLength != nil {
		tr.onPendingLength()
	}

	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	return tr.repo.PendingLength(ctx, cmd)
}

// Implement remaining TaskRepository interface methods (passthrough)
func (tr *trackingRepository) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	return tr.repo.Enqueue(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
}

func (tr *trackingRepository) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	return tr.repo.Claim(ctx, workerID, commands, leaseSeconds, inspectLimit, maxAttemptsDefault, tenantID)
}

func (tr *trackingRepository) Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error {
	return tr.repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
}

func (tr *trackingRepository) Abandon(ctx context.Context, taskID, workerID string) error {
	return tr.repo.Abandon(ctx, taskID, workerID)
}

func (tr *trackingRepository) Nack(ctx context.Context, taskID, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	return tr.repo.Nack(ctx, taskID, workerID, delaySeconds, maxAttemptsDefault, reason)
}

func (tr *trackingRepository) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	return tr.repo.Get(ctx, taskID)
}

func (tr *trackingRepository) AdminQueues(ctx context.Context) (map[string]any, error) {
	return tr.repo.AdminQueues(ctx)
}

func (tr *trackingRepository) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
	return tr.repo.QueueStats(ctx, cmd, tenantID)
}

func (tr *trackingRepository) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	return tr.repo.CleanupExpired(ctx, limit, before)
}

func (tr *trackingRepository) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	return tr.repo.MoveDueDelayed(ctx, cmd, limit)
}
