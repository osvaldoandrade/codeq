package shard

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func setupMigrationClients(t *testing.T) (context.Context, *miniredis.Miniredis, *miniredis.Miniredis, *ClientMap) {
	t.Helper()
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start src: %v", err)
	}
	t.Cleanup(mr1.Close)

	mr2, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start dst: %v", err)
	}
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	cm, err := NewClientMap(map[string]*redis.Client{
		"default":       c1,
		"compute-shard": c2,
	}, "default")
	if err != nil {
		t.Fatalf("new client map: %v", err)
	}
	return context.Background(), mr1, mr2, cm
}

func seedPendingTasks(ctx context.Context, t *testing.T, client *redis.Client, command, tenantID, shardID string, priority int, count int) []string {
	t.Helper()
	key := QueueKeyPending(command, tenantID, shardID, priority)
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("task-%d", i)
		ids[i] = id
		if err := client.LPush(ctx, key, id).Err(); err != nil {
			t.Fatalf("seed pending: %v", err)
		}
		// Seed task data
		if err := client.HSet(ctx, tasksHashKey, id, fmt.Sprintf(`{"id":"%s","command":"%s"}`, id, command)).Err(); err != nil {
			t.Fatalf("seed task data: %v", err)
		}
		// Seed TTL index
		if err := client.ZAdd(ctx, ttlIndexKey, &redis.Z{Score: 1700000000, Member: id}).Err(); err != nil {
			t.Fatalf("seed ttl index: %v", err)
		}
	}
	return ids
}

func seedDelayedTasks(ctx context.Context, t *testing.T, client *redis.Client, command, tenantID, shardID string, count int) []string {
	t.Helper()
	key := QueueKeyDelayed(command, tenantID, shardID)
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("delayed-%d", i)
		ids[i] = id
		score := float64(1700000000 + i)
		if err := client.ZAdd(ctx, key, &redis.Z{Score: score, Member: id}).Err(); err != nil {
			t.Fatalf("seed delayed: %v", err)
		}
		if err := client.HSet(ctx, tasksHashKey, id, fmt.Sprintf(`{"id":"%s"}`, id)).Err(); err != nil {
			t.Fatalf("seed task data: %v", err)
		}
	}
	return ids
}

func seedSetTasks(ctx context.Context, t *testing.T, client *redis.Client, key string, count int, prefix string) []string {
	t.Helper()
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		ids[i] = id
		if err := client.SAdd(ctx, key, id).Err(); err != nil {
			t.Fatalf("seed set: %v", err)
		}
		if err := client.HSet(ctx, tasksHashKey, id, fmt.Sprintf(`{"id":"%s"}`, id)).Err(); err != nil {
			t.Fatalf("seed task data: %v", err)
		}
	}
	return ids
}

func TestMigrate_PendingTasks(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 5, 3)

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.PendingMigrated != 3 {
		t.Errorf("expected 3 pending migrated, got %d", res.PendingMigrated)
	}
	if res.TotalMigrated != 3 {
		t.Errorf("expected 3 total migrated, got %d", res.TotalMigrated)
	}

	// Source should be empty
	srcKey := QueueKeyPending("GENERATE_MASTER", "", "default", 5)
	n, _ := src.LLen(ctx, srcKey).Result()
	if n != 0 {
		t.Errorf("expected 0 remaining on source, got %d", n)
	}

	// Destination should have tasks
	dstKey := QueueKeyPending("GENERATE_MASTER", "", "compute-shard", 5)
	n, _ = dst.LLen(ctx, dstKey).Result()
	if n != 3 {
		t.Errorf("expected 3 on destination, got %d", n)
	}

	// Task data should exist on destination
	js, err := dst.HGet(ctx, tasksHashKey, "task-0").Result()
	if err != nil || js == "" {
		t.Errorf("task data not copied to destination")
	}
}

func TestMigrate_PendingPreservesFIFOOrder(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	// Seed tasks so that RPOP order on source is task-2, task-1, task-0
	// (seedPendingTasks uses LPush, so list is [task-2, task-1, task-0])
	seedPendingTasks(ctx, t, src, "CMD", "", "default", 0, 3)

	// Verify source RPOP order before migration
	srcKey := QueueKeyPending("CMD", "", "default", 0)
	first, _ := src.LIndex(ctx, srcKey, -1).Result() // rightmost = first to pop
	if first != "task-0" {
		t.Fatalf("expected rightmost (first RPOP) to be task-0, got %s", first)
	}

	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "CMD",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify destination RPOP order matches source RPOP order
	dstKey := QueueKeyPending("CMD", "", "compute-shard", 0)
	firstDst, _ := dst.LIndex(ctx, dstKey, -1).Result()
	if firstDst != "task-0" {
		t.Errorf("FIFO broken: expected first RPOP to be task-0, got %s", firstDst)
	}

	// Pop all and verify full order
	var order []string
	for {
		id, err := dst.RPop(ctx, dstKey).Result()
		if err != nil {
			break
		}
		order = append(order, id)
	}
	expected := []string{"task-0", "task-1", "task-2"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d tasks, got %d", len(expected), len(order))
	}
	for i, want := range expected {
		if order[i] != want {
			t.Errorf("position %d: got %s, want %s", i, order[i], want)
		}
	}
}

func TestMigrate_DelayedTasks(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	seedDelayedTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 5)

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 2,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.DelayedMigrated != 5 {
		t.Errorf("expected 5 delayed migrated, got %d", res.DelayedMigrated)
	}

	// Source should be empty
	srcKey := QueueKeyDelayed("GENERATE_MASTER", "", "default")
	n, _ := src.ZCard(ctx, srcKey).Result()
	if n != 0 {
		t.Errorf("expected 0 on source, got %d", n)
	}

	// Destination should have tasks with scores preserved
	dstKey := QueueKeyDelayed("GENERATE_MASTER", "", "compute-shard")
	members, _ := dst.ZRangeWithScores(ctx, dstKey, 0, -1).Result()
	if len(members) != 5 {
		t.Errorf("expected 5 on destination, got %d", len(members))
	}
}

func TestMigrate_DLQTasks(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	dlqKey := QueueKeyDLQ("GENERATE_MASTER", "", "default")
	seedSetTasks(ctx, t, src, dlqKey, 4, "dlq")

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.DLQMigrated != 4 {
		t.Errorf("expected 4 DLQ migrated, got %d", res.DLQMigrated)
	}

	// Destination should have tasks
	dstKey := QueueKeyDLQ("GENERATE_MASTER", "", "compute-shard")
	n, _ := dst.SCard(ctx, dstKey).Result()
	if n != 4 {
		t.Errorf("expected 4 on destination, got %d", n)
	}
}

func TestMigrate_InProgressTasks_BlocksWhenNonEmpty(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	inprogKey := QueueKeyInProgress("GENERATE_MASTER", "", "default")
	seedSetTasks(ctx, t, src, inprogKey, 2, "inprog")

	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err == nil {
		t.Fatal("expected error when in-progress queue is non-empty")
	}
	if got := err.Error(); !contains(got, "still in progress") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMigrate_InProgressTasks_DryRunReportsCounts(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	inprogKey := QueueKeyInProgress("GENERATE_MASTER", "", "default")
	seedSetTasks(ctx, t, src, inprogKey, 2, "inprog")

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		DryRun:    true,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if res.InProgMigrated != 2 {
		t.Errorf("expected dry-run to report 2 in-progress, got %d", res.InProgMigrated)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestMigrate_DryRun(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 0, 5)

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		DryRun:    true,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !res.DryRun {
		t.Error("expected DryRun to be true")
	}
	if res.PendingMigrated != 5 {
		t.Errorf("expected dry-run to report 5, got %d", res.PendingMigrated)
	}

	// Source should still have all tasks (no actual migration)
	srcKey := QueueKeyPending("GENERATE_MASTER", "", "default", 0)
	n, _ := src.LLen(ctx, srcKey).Result()
	if n != 5 {
		t.Errorf("expected 5 still on source after dry-run, got %d", n)
	}
}

func TestMigrate_ProgressCallback(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 0, 3)

	var progressCalls []MigrateProgress
	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 2,
		OnProgress: func(p MigrateProgress) {
			progressCalls = append(progressCalls, p)
		},
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if len(progressCalls) == 0 {
		t.Error("expected at least one progress callback")
	}
	// Last call should show all migrated
	last := progressCalls[len(progressCalls)-1]
	if last.Migrated < 3 {
		t.Errorf("expected last progress to show >= 3 migrated, got %d", last.Migrated)
	}
}

func TestMigrate_SameShardError(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)

	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "default",
	})
	if err == nil {
		t.Error("expected error when source equals destination")
	}
}

func TestMigrate_EmptyQueues(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.TotalMigrated != 0 {
		t.Errorf("expected 0 total migrated, got %d", res.TotalMigrated)
	}
}

func TestMigrate_WithTenant(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "tenant-abc", "default", 0, 2)

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		TenantID:  "tenant-abc",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.PendingMigrated != 2 {
		t.Errorf("expected 2 pending migrated, got %d", res.PendingMigrated)
	}

	// Destination should have tasks under tenant key
	dstKey := QueueKeyPending("GENERATE_MASTER", "tenant-abc", "compute-shard", 0)
	n, _ := dst.LLen(ctx, dstKey).Result()
	if n != 2 {
		t.Errorf("expected 2 on destination, got %d", n)
	}
}

func TestMigrate_TaskDataAndTTLCopied(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")
	dst := cm.Client("compute-shard")

	// Seed with specific task data
	key := QueueKeyPending("GENERATE_MASTER", "", "default", 0)
	taskJSON := `{"id":"task-x","command":"GENERATE_MASTER","payload":"{\"data\":1}"}`
	if err := src.LPush(ctx, key, "task-x").Err(); err != nil {
		t.Fatal(err)
	}
	if err := src.HSet(ctx, tasksHashKey, "task-x", taskJSON).Err(); err != nil {
		t.Fatal(err)
	}
	if err := src.ZAdd(ctx, ttlIndexKey, &redis.Z{Score: 1700086400, Member: "task-x"}).Err(); err != nil {
		t.Fatal(err)
	}

	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Task data should be on destination
	got, err := dst.HGet(ctx, tasksHashKey, "task-x").Result()
	if err != nil {
		t.Fatalf("task data not on destination: %v", err)
	}
	if got != taskJSON {
		t.Errorf("task data mismatch: got %q, want %q", got, taskJSON)
	}

	// TTL index should be on destination
	score, err := dst.ZScore(ctx, ttlIndexKey, "task-x").Result()
	if err != nil {
		t.Fatalf("TTL index not on destination: %v", err)
	}
	if score != 1700086400 {
		t.Errorf("TTL score mismatch: got %f, want 1700086400", score)
	}
}

func TestMigrate_AllQueueTypes(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	// Seed queue types that can be migrated (no in-progress)
	seedPendingTasks(ctx, t, src, "CMD", "", "default", 0, 2)
	seedDelayedTasks(ctx, t, src, "CMD", "", "default", 3)
	dlqKey := QueueKeyDLQ("CMD", "", "default")
	seedSetTasks(ctx, t, src, dlqKey, 2, "dq")

	res, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "CMD",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.PendingMigrated != 2 {
		t.Errorf("pending: got %d, want 2", res.PendingMigrated)
	}
	if res.DelayedMigrated != 3 {
		t.Errorf("delayed: got %d, want 3", res.DelayedMigrated)
	}
	if res.InProgMigrated != 0 {
		t.Errorf("inprog: got %d, want 0", res.InProgMigrated)
	}
	if res.DLQMigrated != 2 {
		t.Errorf("dlq: got %d, want 2", res.DLQMigrated)
	}
	if res.TotalMigrated != 7 {
		t.Errorf("total: got %d, want 7", res.TotalMigrated)
	}
}
