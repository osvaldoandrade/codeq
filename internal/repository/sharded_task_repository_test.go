package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/shard"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestShardedTaskRepository_EnqueueRoutesToCorrectShard(t *testing.T) {
	// Create two separate miniredis instances (simulating two Redis backends)
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 1: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 2: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	clientMap, err := shard.NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("client map: %v", err)
	}

	supplier := &testShardSupplier{
		shardMap:     map[string]string{"GENERATE_MASTER": "compute"},
		defaultShard: "primary",
	}

	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)
	ctx := context.Background()

	// Enqueue GENERATE_MASTER → should go to "compute" shard (mr2)
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":true}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verify task data is on mr2 (compute shard), NOT on mr1 (primary)
	taskJSON, err := c2.HGet(ctx, "codeq:tasks", task.ID).Result()
	if err != nil || taskJSON == "" {
		t.Fatalf("expected task on compute shard (mr2), got err=%v", err)
	}

	// Verify task is NOT on mr1
	_, err = c1.HGet(ctx, "codeq:tasks", task.ID).Result()
	if err == nil {
		t.Fatal("task should NOT be on primary shard (mr1)")
	}

	// Verify the queue key is on mr2
	shardedKey := "codeq:q:generate_master:s:compute:pending:5"
	n, _ := c2.LLen(ctx, shardedKey).Result()
	if n != 1 {
		t.Fatalf("expected 1 item in compute shard pending key, got %d", n)
	}
}

func TestShardedTaskRepository_ClaimFromCorrectShard(t *testing.T) {
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 1: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 2: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	clientMap, err := shard.NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("client map: %v", err)
	}

	supplier := &testShardSupplier{
		shardMap:     map[string]string{"GENERATE_MASTER": "compute"},
		defaultShard: "primary",
	}

	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)
	ctx := context.Background()

	// Enqueue on compute shard
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"claim":"test"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Claim should find the task on the correct shard
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task from sharded queue")
	}
	if claimed.ID != task.ID {
		t.Fatalf("claimed wrong task: got %s, want %s", claimed.ID, task.ID)
	}
}

func TestShardedTaskRepository_GetFansOutToAllShards(t *testing.T) {
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 1: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 2: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	clientMap, err := shard.NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("client map: %v", err)
	}

	supplier := &testShardSupplier{
		shardMap:     map[string]string{"GENERATE_MASTER": "compute"},
		defaultShard: "primary",
	}

	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)
	ctx := context.Background()

	// Enqueue on compute shard
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"get":"test"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Get should fan out and find the task even though we don't know which shard
	found, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found.ID != task.ID {
		t.Fatalf("found wrong task: got %s, want %s", found.ID, task.ID)
	}
}

func TestShardedTaskRepository_GetNotFoundReturnsError(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	clientMap := shard.NewSingleClientMap(c)
	supplier := &testShardSupplier{shardMap: map[string]string{}, defaultShard: "default"}
	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)

	_, err = repo.Get(context.Background(), "nonexistent-id")
	if err == nil || !strings.Contains(err.Error(), "not-found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestShardedTaskRepository_BackwardCompatibleWithSingleShard(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	// Single client map = backward compatible mode
	clientMap := shard.NewSingleClientMap(c)
	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, nil)
	ctx := context.Background()

	// Enqueue with default shard
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"compat":"test"}`, 0, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verify legacy key format is used (no shard segment)
	legacyKey := "codeq:q:generate_master:pending:0"
	n, _ := c.LLen(ctx, legacyKey).Result()
	if n != 1 {
		t.Fatalf("expected 1 item in legacy pending key %s, got %d", legacyKey, n)
	}

	// Claim should work
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task")
	}
	if claimed.ID != task.ID {
		t.Fatalf("claimed wrong task: got %s, want %s", claimed.ID, task.ID)
	}
}

func TestShardedTaskRepository_QueueStatsAggregatesAcrossShards(t *testing.T) {
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 1: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 2: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	clientMap, err := shard.NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("client map: %v", err)
	}

	// Supplier where GENERATE_MASTER routes to "compute" and QueueShards returns both
	supplier := &testShardSupplier{
		shardMap:     map[string]string{"GENERATE_MASTER": "compute"},
		defaultShard: "primary",
	}

	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)
	ctx := context.Background()

	// Enqueue tasks on compute shard
	for i := 0; i < 3; i++ {
		_, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"stats":"test"}`, 5, "", 3, "", time.Time{}, "")
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	stats, err := repo.QueueStats(ctx, domain.CmdGenerateMaster)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	if stats.Ready != 3 {
		t.Fatalf("expected 3 ready tasks, got %d", stats.Ready)
	}
}

func TestShardedTaskRepository_TenantRoutesToDedicatedShard(t *testing.T) {
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 1: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis 2: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	clientMap, err := shard.NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"premium": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("client map: %v", err)
	}

	supplier := &testShardSupplier{
		shardMap:     map[string]string{},
		tenantMap:    map[string]string{"premium-tenant": "premium"},
		defaultShard: "primary",
	}

	repo := NewShardedTaskRepository(clientMap, time.UTC, "exp_full_jitter", 1, 10, supplier)
	ctx := context.Background()

	// Enqueue for premium tenant → should go to premium shard (mr2)
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"tenant":"premium"}`, 0, "", 3, "", time.Time{}, "premium-tenant")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verify task data is on mr2 (premium shard)
	taskJSON, err := c2.HGet(ctx, "codeq:tasks", task.ID).Result()
	if err != nil || taskJSON == "" {
		t.Fatalf("expected task on premium shard (mr2), got err=%v", err)
	}

	// Verify task is NOT on mr1
	_, err = c1.HGet(ctx, "codeq:tasks", task.ID).Result()
	if err == nil {
		t.Fatal("task should NOT be on primary shard (mr1)")
	}
}
