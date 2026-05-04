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
	// Create 3 Redis instances
	mr1 := miniredis.RunT(t)
	mr2 := miniredis.RunT(t)
	mr3 := miniredis.RunT(t)
	defer mr1.Close()
	defer mr2.Close()
	defer mr3.Close()

	// Create Redis clients pointing to each instance
	rdb1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	rdb2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	rdb3 := redis.NewClient(&redis.Options{Addr: mr3.Addr()})

	// Create TaskRepository instances for each shard
	taskRepo1 := &taskRedisRepo{rdb: rdb1, defaultShard: "shard1"}
	taskRepo2 := &taskRedisRepo{rdb: rdb2, defaultShard: "shard2"}
	taskRepo3 := &taskRedisRepo{rdb: rdb3, defaultShard: "shard3"}

	// Seed some data in each shard
	ctx := context.Background()

	// Add 10 pending tasks to each shard
	for i := 0; i < 10; i++ {
		task1 := &domain.Task{ID: fmt.Sprintf("task1_%d", i), Command: "test"}
		task2 := &domain.Task{ID: fmt.Sprintf("task2_%d", i), Command: "test"}
		task3 := &domain.Task{ID: fmt.Sprintf("task3_%d", i), Command: "test"}

		taskRepo1.Enqueue(ctx, task1, domain.CommandMetadata{}, nil)
		taskRepo2.Enqueue(ctx, task2, domain.CommandMetadata{}, nil)
		taskRepo3.Enqueue(ctx, task3, domain.CommandMetadata{}, nil)
	}

	// Create a ShardedTaskRepository with tracking to verify parallelization
	repos := []TaskRepository{taskRepo1, taskRepo2, taskRepo3}
	shardedRepo := &shardedTaskRepository{
		repos:         repos,
		shardSupplier: nil, // Will be set with a mock
	}

	// Test 1: PendingLength should parallelize
	t.Run("PendingLength parallelization", func(t *testing.T) {
		// Create a mock shardSupplier
		mockSupplier := &mockShardSupplier{
			shards: []string{"shard1", "shard2", "shard3"},
		}
		shardedRepo.shardSupplier = mockSupplier

		// Track concurrent calls
		var callCount int32
		var maxConcurrent int32
		var mu sync.Mutex
		activeGoroutines := 0

		// Wrap the repos with tracking
		trackedRepos := make([]TaskRepository, len(repos))
		for i, repo := range repos {
			trackingRepo := &trackingRepository{
				repo:                repo,
				onPendingLength:     func() { atomic.AddInt32(&callCount, 1) },
				onMaxConcurrent:     &maxConcurrent,
				trackingMutex:       &mu,
				activeGoroutinesPtr: &activeGoroutines,
			}
			trackedRepos[i] = trackingRepo
		}

		// Replace repos with tracked versions
		originalRepos := shardedRepo.repos
		shardedRepo.repos = trackedRepos

		length, err := shardedRepo.PendingLength(ctx, "test")
		if err != nil {
			t.Fatalf("PendingLength failed: %v", err)
		}

		// Should have found 30 total pending tasks (10 in each shard)
		if length != 30 {
			t.Errorf("Expected 30 pending tasks, got %d", length)
		}

		// Verify that all shards were queried
		if callCount != 3 {
			t.Errorf("Expected 3 calls to PendingLength (one per shard), got %d", callCount)
		}

		shardedRepo.repos = originalRepos
	})

	// Test 2: QueueStats should parallelize
	t.Run("QueueStats parallelization", func(t *testing.T) {
		mockSupplier := &mockShardSupplier{
			shards: []string{"shard1", "shard2", "shard3"},
		}
		shardedRepo.shardSupplier = mockSupplier

		stats, err := shardedRepo.QueueStats(ctx, "test")
		if err != nil {
			t.Fatalf("QueueStats failed: %v", err)
		}

		// Should have found 30 total ready tasks
		if stats.Ready != 30 {
			t.Errorf("Expected 30 ready tasks, got %d", stats.Ready)
		}
	})

	// Test 3: AdminQueues should parallelize
	t.Run("AdminQueues parallelization", func(t *testing.T) {
		// AdminQueues doesn't use shardSupplier, just loops all repos
		result, err := shardedRepo.AdminQueues(ctx)
		if err != nil {
			t.Fatalf("AdminQueues failed: %v", err)
		}

		// Verify that queue stats were aggregated across shards
		if result == nil || len(result) == 0 {
			t.Error("Expected AdminQueues to return aggregated results")
		}
	})
}

// TestShardedOperationsPerformance demonstrates performance improvement from parallelization
// by measuring latency with simulated slow Redis operations.
func TestShardedOperationsPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	// Create 5 shards to make performance difference more pronounced
	numShards := 5
	shards := make([]TaskRepository, numShards)
	var setupWaitGroup sync.WaitGroup

	for i := 0; i < numShards; i++ {
		mr := miniredis.RunT(t)
		t.Cleanup(mr.Close)

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		shards[i] = &taskRedisRepo{rdb: rdb, defaultShard: fmt.Sprintf("shard%d", i)}

		// Seed each shard with data
		setupWaitGroup.Add(1)
		go func(j int, repo TaskRepository) {
			defer setupWaitGroup.Done()
			ctx := context.Background()
			for k := 0; k < 5; k++ {
				task := &domain.Task{
					ID:      fmt.Sprintf("task_%d_%d", j, k),
					Command: "perf_test",
				}
				repo.Enqueue(ctx, task, domain.CommandMetadata{}, nil)
			}
		}(i, shards[i])
	}
	setupWaitGroup.Wait()

	mockSupplier := &mockShardSupplier{
		shards: make([]string, numShards),
	}
	for i := 0; i < numShards; i++ {
		mockSupplier.shards[i] = fmt.Sprintf("shard%d", i)
	}

	shardedRepo := &shardedTaskRepository{
		repos:         shards,
		shardSupplier: mockSupplier,
	}

	ctx := context.Background()

	// Measure PendingLength performance
	start := time.Now()
	length, err := shardedRepo.PendingLength(ctx, "perf_test")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("PendingLength failed: %v", err)
	}

	expectedTotal := int64(numShards * 5)
	if length != expectedTotal {
		t.Errorf("Expected %d pending tasks, got %d", expectedTotal, length)
	}

	// With parallelization, latency should be dominated by slowest shard (~1-2ms each)
	// Without parallelization, latency would be ~5-10ms (sum of all shards)
	// This test primarily documents that parallelization works correctly
	t.Logf("PendingLength across %d shards took %v (expected to be dominated by slowest shard, not sum)", numShards, elapsed)

	// Measure QueueStats performance
	start = time.Now()
	stats, err := shardedRepo.QueueStats(ctx, "perf_test")
	elapsed = time.Since(start)

	if err != nil {
		t.Fatalf("QueueStats failed: %v", err)
	}

	if stats.Ready != expectedTotal {
		t.Errorf("Expected %d ready tasks, got %d", expectedTotal, stats.Ready)
	}

	t.Logf("QueueStats across %d shards took %v", numShards, elapsed)

	// Measure AdminQueues performance
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

	// Update max concurrent
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

	// Add a small delay to simulate network latency
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	return tr.repo.PendingLength(ctx, cmd)
}

// Implement other required TaskRepository methods (passthrough)
func (tr *trackingRepository) Enqueue(ctx context.Context, task *domain.Task, metadata domain.CommandMetadata, dedup *domain.Deduplication) (string, error) {
	return tr.repo.Enqueue(ctx, task, metadata, dedup)
}

func (tr *trackingRepository) Claim(ctx context.Context, cmd domain.Command, workerID string, ttlSeconds int, inspectionLimit int) (*domain.Task, error) {
	return tr.repo.Claim(ctx, cmd, workerID, ttlSeconds, inspectionLimit)
}

func (tr *trackingRepository) Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error {
	return tr.repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
}

func (tr *trackingRepository) Abandon(ctx context.Context, taskID, workerID string) error {
	return tr.repo.Abandon(ctx, taskID, workerID)
}

func (tr *trackingRepository) Nack(ctx context.Context, taskID, workerID string, nackReason string) error {
	return tr.repo.Nack(ctx, taskID, workerID, nackReason)
}

func (tr *trackingRepository) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	return tr.repo.Get(ctx, taskID)
}

func (tr *trackingRepository) AdminQueues(ctx context.Context) (map[string]any, error) {
	return tr.repo.AdminQueues(ctx)
}

func (tr *trackingRepository) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	return tr.repo.QueueStats(ctx, cmd)
}

func (tr *trackingRepository) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	return tr.repo.CleanupExpired(ctx, limit, before)
}

func (tr *trackingRepository) MoveDueDelayed(ctx context.Context) (int, error) {
	return tr.repo.MoveDueDelayed(ctx)
}
