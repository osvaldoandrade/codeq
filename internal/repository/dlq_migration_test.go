package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

// TestDLQMigrationOptionA_DrainAndCutover validates the drain-and-cut-over
// migration path: workers drain the legacy LIST-based DLQ, then the system
// switches to using SET-based DLQ for new entries.
func TestDLQMigrationOptionA_DrainAndCutover(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"

	// Step 1: Simulate pre-migration state — DLQ is a LIST with task IDs.
	legacyIDs := []string{"task-aaa", "task-bbb", "task-ccc"}
	for _, id := range legacyIDs {
		if err := rdb.LPush(ctx, dlqKey, id).Err(); err != nil {
			t.Fatalf("LPUSH legacy DLQ: %v", err)
		}
	}
	if n, _ := rdb.LLen(ctx, dlqKey).Result(); n != int64(len(legacyIDs)) {
		t.Fatalf("expected %d items in legacy DLQ LIST, got %d", len(legacyIDs), n)
	}

	// Step 2: Drain the legacy LIST (Option A).
	drained := make([]string, 0, len(legacyIDs))
	for {
		val, err := rdb.RPop(ctx, dlqKey).Result()
		if err == redis.Nil {
			break
		}
		if err != nil {
			t.Fatalf("RPop drain: %v", err)
		}
		drained = append(drained, val)
	}
	if len(drained) != len(legacyIDs) {
		t.Fatalf("expected to drain %d items, got %d", len(legacyIDs), len(drained))
	}

	// Step 3: Verify DLQ key is gone.
	exists, _ := rdb.Exists(ctx, dlqKey).Result()
	if exists != 0 {
		t.Fatalf("expected DLQ key to be deleted after drain")
	}

	// Step 4: Deploy new SET-based DLQ — use the real repository.
	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	task, err := repo.Enqueue(ctx, cmd, `{"migrate":"optionA"}`, 0, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 1, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	_, dlq, err := repo.Nack(ctx, claimed.ID, "worker-1", 0, 1, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if !dlq {
		t.Fatalf("expected task to go to DLQ")
	}

	// Step 5: Verify new DLQ is a SET with exactly 1 item.
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != 1 {
		t.Fatalf("expected 1 item in new SET DLQ, got %d", n)
	}
	if isMember, _ := rdb.SIsMember(ctx, dlqKey, task.ID).Result(); !isMember {
		t.Fatalf("expected task %s to be in DLQ SET", task.ID)
	}
}

// TestDLQMigrationOptionB_InPlaceConversion validates the in-place LIST→SET
// conversion: LRANGE → SADD → DEL as documented in migration.md.
func TestDLQMigrationOptionB_InPlaceConversion(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"
	dlqBackupKey := dlqKey + "_list"

	// Step 1: Populate legacy LIST-based DLQ.
	legacyIDs := []string{"task-111", "task-222", "task-333", "task-444", "task-555"}
	for _, id := range legacyIDs {
		if err := rdb.LPush(ctx, dlqKey, id).Err(); err != nil {
			t.Fatalf("LPUSH legacy DLQ: %v", err)
		}
	}

	// Step 2: RENAME the LIST key to a backup.
	if err := rdb.Rename(ctx, dlqKey, dlqBackupKey).Err(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}

	// Step 3: LRANGE all IDs from the backup.
	ids, err := rdb.LRange(ctx, dlqBackupKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE: %v", err)
	}
	if len(ids) != len(legacyIDs) {
		t.Fatalf("expected %d items from LRANGE, got %d", len(legacyIDs), len(ids))
	}

	// Step 4: SADD all IDs into the new SET key.
	members := make([]interface{}, len(ids))
	for i, id := range ids {
		members[i] = id
	}
	if err := rdb.SAdd(ctx, dlqKey, members...).Err(); err != nil {
		t.Fatalf("SADD: %v", err)
	}

	// Step 5: DEL the backup LIST.
	if err := rdb.Del(ctx, dlqBackupKey).Err(); err != nil {
		t.Fatalf("DEL backup: %v", err)
	}

	// Step 6: Verify the new key is a SET with all original IDs.
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != int64(len(legacyIDs)) {
		t.Fatalf("expected %d items in SET DLQ, got %d", len(legacyIDs), n)
	}
	for _, id := range legacyIDs {
		if isMember, _ := rdb.SIsMember(ctx, dlqKey, id).Result(); !isMember {
			t.Fatalf("expected %s to be in DLQ SET after migration", id)
		}
	}

	// Step 7: Verify repository can operate on the migrated SET.
	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	task, err := repo.Enqueue(ctx, cmd, `{"migrate":"optionB"}`, 0, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 1, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	_, dlq, err := repo.Nack(ctx, claimed.ID, "worker-1", 0, 1, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if !dlq {
		t.Fatalf("expected task to go to DLQ")
	}

	// The SET should now contain original IDs + new task ID.
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != int64(len(legacyIDs)+1) {
		t.Fatalf("expected %d items in DLQ after new nack, got %d", len(legacyIDs)+1, n)
	}
	if isMember, _ := rdb.SIsMember(ctx, dlqKey, task.ID).Result(); !isMember {
		t.Fatalf("expected new task %s to be in DLQ SET", task.ID)
	}
}

// TestInProgressMigrationOptionB_InPlaceConversion validates the in-place
// LIST→SET conversion for the in-progress queue.
func TestInProgressMigrationOptionB_InPlaceConversion(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	inprogKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":inprog"
	inprogBackupKey := inprogKey + "_list"

	// Step 1: Populate legacy LIST-based in-progress queue.
	legacyIDs := []string{"task-aaa", "task-bbb", "task-ccc"}
	for _, id := range legacyIDs {
		if err := rdb.LPush(ctx, inprogKey, id).Err(); err != nil {
			t.Fatalf("LPUSH legacy inprog: %v", err)
		}
	}

	// Step 2: RENAME to backup.
	if err := rdb.Rename(ctx, inprogKey, inprogBackupKey).Err(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}

	// Step 3: LRANGE all IDs.
	ids, err := rdb.LRange(ctx, inprogBackupKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE: %v", err)
	}

	// Step 4: SADD into new SET.
	members := make([]interface{}, len(ids))
	for i, id := range ids {
		members[i] = id
	}
	if err := rdb.SAdd(ctx, inprogKey, members...).Err(); err != nil {
		t.Fatalf("SADD: %v", err)
	}

	// Step 5: DEL backup.
	if err := rdb.Del(ctx, inprogBackupKey).Err(); err != nil {
		t.Fatalf("DEL backup: %v", err)
	}

	// Step 6: Verify SET integrity.
	if n, _ := rdb.SCard(ctx, inprogKey).Result(); n != int64(len(legacyIDs)) {
		t.Fatalf("expected %d items in inprog SET, got %d", len(legacyIDs), n)
	}
	for _, id := range legacyIDs {
		if isMember, _ := rdb.SIsMember(ctx, inprogKey, id).Result(); !isMember {
			t.Fatalf("expected %s to be in inprog SET after migration", id)
		}
	}
}

// TestDLQMigrationEmptyQueue verifies migration succeeds when the DLQ is empty.
func TestDLQMigrationEmptyQueue(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"

	// DLQ key does not exist — simulate fresh deploy or already drained.
	exists, _ := rdb.Exists(ctx, dlqKey).Result()
	if exists != 0 {
		t.Fatalf("expected DLQ key to not exist initially")
	}

	// New SET-based operations should work on non-existent key.
	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	task, err := repo.Enqueue(ctx, cmd, `{"empty":"dlq"}`, 0, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 1, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	_, dlq, err := repo.Nack(ctx, task.ID, "worker-1", 0, 1, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if !dlq {
		t.Fatalf("expected task to go to DLQ")
	}
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != 1 {
		t.Fatalf("expected 1 item in DLQ SET, got %d", n)
	}
}

// TestDLQMigrationLargeQueue validates in-place conversion with a large number
// of DLQ entries to ensure LRANGE + SADD handles bulk data correctly.
func TestDLQMigrationLargeQueue(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"
	dlqBackupKey := dlqKey + "_list"

	// Populate a large LIST-based DLQ.
	const count = 1000
	expectedIDs := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("task-%04d", i)
		expectedIDs[id] = true
		if err := rdb.LPush(ctx, dlqKey, id).Err(); err != nil {
			t.Fatalf("LPUSH: %v", err)
		}
	}

	// In-place migration: RENAME → LRANGE → SADD → DEL
	if err := rdb.Rename(ctx, dlqKey, dlqBackupKey).Err(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	ids, err := rdb.LRange(ctx, dlqBackupKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE: %v", err)
	}
	if len(ids) != count {
		t.Fatalf("expected %d items from LRANGE, got %d", count, len(ids))
	}
	members := make([]interface{}, len(ids))
	for i, id := range ids {
		members[i] = id
	}
	if err := rdb.SAdd(ctx, dlqKey, members...).Err(); err != nil {
		t.Fatalf("SADD: %v", err)
	}
	if err := rdb.Del(ctx, dlqBackupKey).Err(); err != nil {
		t.Fatalf("DEL backup: %v", err)
	}

	// Verify all IDs preserved.
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != count {
		t.Fatalf("expected %d items in SET DLQ, got %d", count, n)
	}
	setMembers, _ := rdb.SMembers(ctx, dlqKey).Result()
	for _, m := range setMembers {
		if !expectedIDs[m] {
			t.Fatalf("unexpected member in DLQ SET: %s", m)
		}
	}
}

// TestDLQMigrationTaskIntegrity verifies that task JSON data stored in the
// tasks hash is preserved and accessible after DLQ migration.
func TestDLQMigrationTaskIntegrity(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster

	// Create task, claim, and nack to DLQ using repository.
	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	task, err := repo.Enqueue(ctx, cmd, `{"payload":"important-data"}`, 0, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 1, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	_, dlq, err := repo.Nack(ctx, task.ID, "worker-1", 0, 1, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if !dlq {
		t.Fatalf("expected task to go to DLQ")
	}

	// Verify task data is intact after landing in DLQ.
	stored, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("expected status=%s, got %s", domain.StatusFailed, stored.Status)
	}
	if stored.LastKnownLocation != domain.LocationDLQ {
		t.Fatalf("expected location=%s, got %s", domain.LocationDLQ, stored.LastKnownLocation)
	}
	if stored.Payload != `{"payload":"important-data"}` {
		t.Fatalf("expected payload preserved, got %s", stored.Payload)
	}
	if stored.Command != cmd {
		t.Fatalf("expected command=%s, got %s", cmd, stored.Command)
	}
	if stored.Attempts != 2 {
		t.Fatalf("expected attempts=2 (incremented by nack), got %d", stored.Attempts)
	}
}

// TestDLQMigrationRollback validates the rollback procedure: converting the
// SET-based DLQ back to a LIST if a rollback is needed.
func TestDLQMigrationRollback(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"
	dlqBackupKey := dlqKey + "_set"

	// Step 1: Populate SET-based DLQ (post-migration state).
	setIDs := []string{"task-new-1", "task-new-2", "task-new-3"}
	members := make([]interface{}, len(setIDs))
	for i, id := range setIDs {
		members[i] = id
	}
	if err := rdb.SAdd(ctx, dlqKey, members...).Err(); err != nil {
		t.Fatalf("SADD: %v", err)
	}

	// Step 2: Rollback — SMEMBERS → RENAME → LPUSH
	ids, err := rdb.SMembers(ctx, dlqKey).Result()
	if err != nil {
		t.Fatalf("SMEMBERS: %v", err)
	}
	if err := rdb.Rename(ctx, dlqKey, dlqBackupKey).Err(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	for _, id := range ids {
		if err := rdb.LPush(ctx, dlqKey, id).Err(); err != nil {
			t.Fatalf("LPUSH rollback: %v", err)
		}
	}
	if err := rdb.Del(ctx, dlqBackupKey).Err(); err != nil {
		t.Fatalf("DEL backup: %v", err)
	}

	// Step 3: Verify the key is now a LIST with all original IDs.
	if n, _ := rdb.LLen(ctx, dlqKey).Result(); n != int64(len(setIDs)) {
		t.Fatalf("expected %d items in rolled-back LIST DLQ, got %d", len(setIDs), n)
	}
	listIDs, err := rdb.LRange(ctx, dlqKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE: %v", err)
	}
	idSet := make(map[string]bool, len(setIDs))
	for _, id := range setIDs {
		idSet[id] = true
	}
	for _, id := range listIDs {
		if !idSet[id] {
			t.Fatalf("unexpected ID in rolled-back LIST: %s", id)
		}
	}
}

// TestDLQMigrationConcurrentAccess verifies that SET-based DLQ handles
// concurrent SADD and SREM operations correctly (simulating a migration window
// where new tasks are nacked while old tasks are cleaned up).
func TestDLQMigrationConcurrentAccess(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"

	// Pre-populate SET DLQ with some IDs.
	const preExisting = 10
	for i := 0; i < preExisting; i++ {
		id := fmt.Sprintf("pre-task-%d", i)
		if err := rdb.SAdd(ctx, dlqKey, id).Err(); err != nil {
			t.Fatalf("SADD pre-populate: %v", err)
		}
	}

	// Concurrently SADD new items and SREM existing items.
	var wg sync.WaitGroup
	const concurrentAdds = 20
	const concurrentRems = 5

	// Add new items concurrently.
	for i := 0; i < concurrentAdds; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("new-task-%d", idx)
			_ = rdb.SAdd(ctx, dlqKey, id).Err()
		}(i)
	}

	// Remove some pre-existing items concurrently.
	for i := 0; i < concurrentRems; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("pre-task-%d", idx)
			_ = rdb.SRem(ctx, dlqKey, id).Err()
		}(i)
	}

	wg.Wait()

	// Verify final count: preExisting - concurrentRems + concurrentAdds
	expected := int64(preExisting - concurrentRems + concurrentAdds)
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != expected {
		t.Fatalf("expected %d items in DLQ after concurrent ops, got %d", expected, n)
	}
}

// TestDLQMigrationDuplicateProtection verifies that SET-based DLQ prevents
// duplicate task IDs, which was a potential issue with the LIST-based approach.
func TestDLQMigrationDuplicateProtection(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	cmd := domain.CmdGenerateMaster
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"

	// SADD the same task ID multiple times.
	for i := 0; i < 5; i++ {
		rdb.SAdd(ctx, dlqKey, "duplicate-task")
	}

	// SET should contain exactly 1 entry (duplicates prevented).
	if n, _ := rdb.SCard(ctx, dlqKey).Result(); n != 1 {
		t.Fatalf("expected 1 item in DLQ SET (no duplicates), got %d", n)
	}
}

// TestDLQMigrationVerificationChecklist validates the verification steps from
// the migration guide checklist: task hash, queue counts, admin endpoint.
func TestDLQMigrationVerificationChecklist(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster

	// Enqueue, claim, and nack to populate all queues.
	task, err := repo.Enqueue(ctx, cmd, `{"verify":"checklist"}`, 0, "", 2, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Verification 1: codeq:tasks hash contains task JSON.
	taskJSON, err := rdb.HGet(ctx, "codeq:tasks", task.ID).Result()
	if err != nil {
		t.Fatalf("tasks hash missing task: %v", err)
	}
	if taskJSON == "" {
		t.Fatal("expected non-empty task JSON in codeq:tasks hash")
	}

	// Verification 2: pending queue has expected count.
	pendingKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":pending:0"
	if n, _ := rdb.LLen(ctx, pendingKey).Result(); n != 1 {
		t.Fatalf("expected 1 pending task, got %d", n)
	}

	// Claim and nack to test the full lifecycle.
	_, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 2, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// Verification 3: lease key exists for in-progress task.
	leaseKey := "codeq:lease:" + task.ID
	if ttl, _ := rdb.TTL(ctx, leaseKey).Result(); ttl <= 0 {
		t.Fatalf("expected positive TTL on lease key, got %v", ttl)
	}

	// Verification 4: AdminQueues returns non-zero counts.
	queues, err := repo.AdminQueues(ctx)
	if err != nil {
		t.Fatalf("admin queues: %v", err)
	}
	if queues == nil {
		t.Fatal("expected non-nil admin queues")
	}

	// Verification 5: QueueStats reflects in-progress task.
	stats, err := repo.QueueStats(ctx, cmd)
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	if stats.InProgress < 1 {
		t.Fatalf("expected at least 1 in-progress task, got %d", stats.InProgress)
	}
}
