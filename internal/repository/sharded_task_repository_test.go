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

func TestShardedTaskRepository_HeartbeatFansOutToAllShards(t *testing.T) {
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

	// Enqueue on compute shard, then claim
	_, err = repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"hb":"test"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task")
	}

	// Heartbeat should fan out and find the task on compute shard
	err = repo.Heartbeat(ctx, claimed.ID, "worker-1", 120)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// Heartbeat with wrong worker should fail
	err = repo.Heartbeat(ctx, claimed.ID, "wrong-worker", 120)
	if err == nil {
		t.Fatal("expected error for wrong worker")
	}
}

func TestShardedTaskRepository_HeartbeatNotFoundReturnsError(t *testing.T) {
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

	err = repo.Heartbeat(context.Background(), "nonexistent-id", "worker-1", 60)
	if err == nil || !strings.Contains(err.Error(), "not-found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestShardedTaskRepository_AbandonFansOutToAllShards(t *testing.T) {
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

	// Enqueue on compute shard, then claim
	_, err = repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"abandon":"test"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task")
	}

	// Abandon should fan out and find the task on compute shard
	err = repo.Abandon(ctx, claimed.ID, "worker-1")
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}

	// After abandon, task should be claimable again
	reClaimed, ok, err := repo.Claim(ctx, "worker-2", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !ok || reClaimed == nil {
		t.Fatal("expected to re-claim task after abandon")
	}
	if reClaimed.ID != claimed.ID {
		t.Fatalf("re-claimed wrong task: got %s, want %s", reClaimed.ID, claimed.ID)
	}
}

func TestShardedTaskRepository_NackFansOutToAllShards(t *testing.T) {
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

	// Enqueue on compute shard, then claim
	_, err = repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"nack":"test"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 5, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task")
	}

	// Nack should fan out and find the task on compute shard
	delay, dlq, err := repo.Nack(ctx, claimed.ID, "worker-1", 0, 5, "TEST_REASON")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if dlq {
		t.Fatal("did not expect DLQ on first nack")
	}
	if delay != 0 {
		t.Fatalf("expected delay=0, got %d", delay)
	}
}

func TestShardedTaskRepository_NackToDLQAcrossShards(t *testing.T) {
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

	repo := NewShardedTaskRepository(clientMap, time.UTC, "fixed", 0, 1, supplier)
	ctx := context.Background()

	// Enqueue with maxAttempts=1 so first nack moves to DLQ
	_, err = repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"dlq":"test"}`, 5, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 10, 1, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || claimed == nil {
		t.Fatal("expected to claim task")
	}

	// Nack should move to DLQ since maxAttempts=1
	_, dlq, err := repo.Nack(ctx, claimed.ID, "worker-1", 0, 1, "MAX_ATTEMPTS")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if !dlq {
		t.Fatal("expected task to be moved to DLQ")
	}

	// Verify task is in DLQ on compute shard (mr2)
	dlqKey := "codeq:q:generate_master:s:compute:dlq"
	isMember, _ := c2.SIsMember(ctx, dlqKey, claimed.ID).Result()
	if !isMember {
		t.Fatalf("expected task in DLQ key %s on compute shard", dlqKey)
	}
}

func TestShardedTaskRepository_AdminQueuesAggregatesAcrossShards(t *testing.T) {
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
	for i := 0; i < 2; i++ {
		_, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"admin":"test"}`, 5, "", 3, "", time.Time{}, "")
		if err != nil {
			t.Fatalf("enqueue compute %d: %v", i, err)
		}
	}

	// Enqueue on primary shard (GENERATE_CREATIVE goes to default)
	for i := 0; i < 3; i++ {
		_, err := repo.Enqueue(ctx, domain.CmdGenerateCreative, `{"admin":"test"}`, 0, "", 3, "", time.Time{}, "")
		if err != nil {
			t.Fatalf("enqueue primary %d: %v", i, err)
		}
	}

	result, err := repo.AdminQueues(ctx)
	if err != nil {
		t.Fatalf("admin queues: %v", err)
	}

	// Should contain entries from both shards
	if len(result) == 0 {
		t.Fatal("expected non-empty admin queues result")
	}

	// Check compute shard pending key
	computeKey := "codeq:q:generate_master:s:compute:pending:5"
	if v, ok := result[computeKey]; ok {
		if count, ok := v.(int64); ok && count != 2 {
			t.Fatalf("expected 2 in %s, got %d", computeKey, count)
		}
	}

	// Check primary shard pending key
	primaryKey := "codeq:q:generate_creative:pending:0"
	if v, ok := result[primaryKey]; ok {
		if count, ok := v.(int64); ok && count != 3 {
			t.Fatalf("expected 3 in %s, got %d", primaryKey, count)
		}
	}
}

func TestShardedTaskRepository_PendingLengthAggregatesAcrossShards(t *testing.T) {
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

	// Enqueue 4 tasks on compute shard
	for i := 0; i < 4; i++ {
		_, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"pending":"test"}`, 5, "", 3, "", time.Time{}, "")
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	total, err := repo.PendingLength(ctx, domain.CmdGenerateMaster)
	if err != nil {
		t.Fatalf("pending length: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected pending length 4, got %d", total)
	}
}

func TestShardedTaskRepository_CleanupExpiredAcrossShards(t *testing.T) {
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
	_, err = repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"cleanup":"compute"}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue compute: %v", err)
	}

	// Enqueue on primary shard
	_, err = repo.Enqueue(ctx, domain.CmdGenerateCreative, `{"cleanup":"primary"}`, 0, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue primary: %v", err)
	}

	// Cleanup with a future time should remove expired tasks from both shards
	deleted, err := repo.CleanupExpired(ctx, 100, time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("expected at least 2 tasks cleaned up across shards, got %d", deleted)
	}
}

func TestShardedTaskRepository_MoveDueDelayedOnCorrectShard(t *testing.T) {
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

	// Enqueue with a future visibleAt so it goes to delayed queue
	futureTime := time.Now().Add(1 * time.Second)
	task, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"delayed":"test"}`, 5, "", 3, "", futureTime, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verify task is in delayed queue on compute shard
	delayedKey := "codeq:q:generate_master:s:compute:delayed"
	delayedCount, _ := c2.ZCard(ctx, delayedKey).Result()
	if delayedCount != 1 {
		t.Fatalf("expected 1 in delayed queue on compute shard, got %d", delayedCount)
	}

	// Verify task is NOT on primary shard's delayed queue
	primaryDelayedKey := "codeq:q:generate_master:delayed"
	primaryDelayed, _ := c1.ZCard(ctx, primaryDelayedKey).Result()
	if primaryDelayed != 0 {
		t.Fatalf("expected 0 in primary shard delayed queue, got %d", primaryDelayed)
	}

	// Manually adjust the score to the past so MoveDueDelayed picks it up
	c2.ZAdd(ctx, delayedKey, &redis.Z{
		Score:  float64(time.Now().Add(-1 * time.Minute).UTC().Unix()),
		Member: task.ID,
	})

	// MoveDueDelayed should operate on the correct shard
	moved, err := repo.MoveDueDelayed(ctx, domain.CmdGenerateMaster, 10)
	if err != nil {
		t.Fatalf("move due delayed: %v", err)
	}
	if moved != 1 {
		t.Fatalf("expected 1 moved, got %d", moved)
	}

	// Task should now be in pending on compute shard
	pendingKey := "codeq:q:generate_master:s:compute:pending:5"
	pendingCount, _ := c2.LLen(ctx, pendingKey).Result()
	if pendingCount != 1 {
		t.Fatalf("expected 1 in pending queue on compute shard, got %d", pendingCount)
	}
}
