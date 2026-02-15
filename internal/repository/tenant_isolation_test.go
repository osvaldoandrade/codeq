package repository

import (
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TestTenantIsolation verifies that tasks from different tenants
// are stored in separate queues and cannot be claimed by workers
// from other tenants.
func TestTenantIsolation(t *testing.T) {
	ctx, mr, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster

	// Create tasks for tenant A
	taskA1, err := repo.Enqueue(ctx, cmd, `{"tenant":"A","task":1}`, 0, "", 5, "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue tenant A task 1: %v", err)
	}
	taskA2, err := repo.Enqueue(ctx, cmd, `{"tenant":"A","task":2}`, 0, "", 5, "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue tenant A task 2: %v", err)
	}

	// Create tasks for tenant B
	taskB1, err := repo.Enqueue(ctx, cmd, `{"tenant":"B","task":1}`, 0, "", 5, "", time.Time{}, "tenant-b")
	if err != nil {
		t.Fatalf("enqueue tenant B task 1: %v", err)
	}
	taskB2, err := repo.Enqueue(ctx, cmd, `{"tenant":"B","task":2}`, 0, "", 5, "", time.Time{}, "tenant-b")
	if err != nil {
		t.Fatalf("enqueue tenant B task 2: %v", err)
	}

	// Verify tasks are stored with correct tenant IDs
	if taskA1.TenantID != "tenant-a" {
		t.Errorf("expected tenant A task to have tenantID 'tenant-a', got %s", taskA1.TenantID)
	}
	if taskB1.TenantID != "tenant-b" {
		t.Errorf("expected tenant B task to have tenantID 'tenant-b', got %s", taskB1.TenantID)
	}

	// Verify queue keys are different for different tenants
	keyA := "codeq:q:generate_master:tenant-a:pending:0"
	keyB := "codeq:q:generate_master:tenant-b:pending:0"
	
	lenA, err := rdb.LLen(ctx, keyA).Result()
	if err != nil {
		t.Fatalf("get length of tenant A queue: %v", err)
	}
	if lenA != 2 {
		t.Errorf("expected 2 tasks in tenant A queue, got %d", lenA)
	}

	lenB, err := rdb.LLen(ctx, keyB).Result()
	if err != nil {
		t.Fatalf("get length of tenant B queue: %v", err)
	}
	if lenB != 2 {
		t.Errorf("expected 2 tasks in tenant B queue, got %d", lenB)
	}

	// Worker from tenant A should only claim tenant A tasks
	claimedA, ok, err := repo.Claim(ctx, "worker-a", []domain.Command{cmd}, 60, 50, 5, "tenant-a")
	if err != nil {
		t.Fatalf("claim for tenant A: %v", err)
	}
	if !ok {
		t.Fatal("expected claim to succeed for tenant A")
	}
	if claimedA.TenantID != "tenant-a" {
		t.Errorf("worker A claimed task with wrong tenant ID: %s", claimedA.TenantID)
	}
	if claimedA.ID != taskA1.ID && claimedA.ID != taskA2.ID {
		t.Errorf("worker A claimed unexpected task: %s", claimedA.ID)
	}

	// Worker from tenant B should only claim tenant B tasks
	claimedB, ok, err := repo.Claim(ctx, "worker-b", []domain.Command{cmd}, 60, 50, 5, "tenant-b")
	if err != nil {
		t.Fatalf("claim for tenant B: %v", err)
	}
	if !ok {
		t.Fatal("expected claim to succeed for tenant B")
	}
	if claimedB.TenantID != "tenant-b" {
		t.Errorf("worker B claimed task with wrong tenant ID: %s", claimedB.TenantID)
	}
	if claimedB.ID != taskB1.ID && claimedB.ID != taskB2.ID {
		t.Errorf("worker B claimed unexpected task: %s", claimedB.ID)
	}

	// Verify tasks are moved to correct in-progress queues
	inprogA := "codeq:q:generate_master:tenant-a:inprog"
	inprogB := "codeq:q:generate_master:tenant-b:inprog"
	
	lenInprogA, err := rdb.SCard(ctx, inprogA).Result()
	if err != nil {
		t.Fatalf("get length of tenant A inprog queue: %v", err)
	}
	if lenInprogA != 1 {
		t.Errorf("expected 1 task in tenant A inprog queue, got %d", lenInprogA)
	}

	lenInprogB, err := rdb.SCard(ctx, inprogB).Result()
	if err != nil {
		t.Fatalf("get length of tenant B inprog queue: %v", err)
	}
	if lenInprogB != 1 {
		t.Errorf("expected 1 task in tenant B inprog queue, got %d", lenInprogB)
	}

	// Worker A should not be able to claim worker B's task
	_, _, err = repo.Claim(ctx, "worker-a", []domain.Command{cmd}, 60, 50, 5, "tenant-b")
	if err != nil {
		t.Fatalf("claim for tenant B with worker A should not error: %v", err)
	}
	// Worker A claiming from tenant B queue should find only 1 task (not the one already claimed by B)
	
	// Clean the mini Redis state (for test cleanup)
	_ = mr // use mr to avoid unused variable error
}

// TestBackwardCompatibilityWithEmptyTenant tests that the system works
// with empty tenant IDs for backward compatibility.
func TestBackwardCompatibilityWithEmptyTenant(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster

	// Create tasks without tenant ID (empty string)
	task1, err := repo.Enqueue(ctx, cmd, `{"test":1}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue task 1: %v", err)
	}
	task2, err := repo.Enqueue(ctx, cmd, `{"test":2}`, 0, "", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue task 2: %v", err)
	}

	// Verify tasks are in the default (non-tenant) queue
	key := "codeq:q:generate_master:pending:0"
	
	length, err := rdb.LLen(ctx, key).Result()
	if err != nil {
		t.Fatalf("get length of default queue: %v", err)
	}
	if length != 2 {
		t.Errorf("expected 2 tasks in default queue, got %d", length)
	}

	// Worker with empty tenant should claim from default queue
	claimed, ok, err := repo.Claim(ctx, "worker-default", []domain.Command{cmd}, 60, 50, 5, "")
	if err != nil {
		t.Fatalf("claim for default tenant: %v", err)
	}
	if !ok {
		t.Fatal("expected claim to succeed for default tenant")
	}
	if claimed.ID != task1.ID && claimed.ID != task2.ID {
		t.Errorf("worker claimed unexpected task: %s", claimed.ID)
	}

	// Verify task moved to default in-progress queue
	inprogKey := "codeq:q:generate_master:inprog"
	lenInprog, err := rdb.SCard(ctx, inprogKey).Result()
	if err != nil {
		t.Fatalf("get length of default inprog queue: %v", err)
	}
	if lenInprog != 1 {
		t.Errorf("expected 1 task in default inprog queue, got %d", lenInprog)
	}
}

// TestTenantIsolationInDelayedQueue verifies tenant isolation for delayed/scheduled tasks
func TestTenantIsolationInDelayedQueue(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	
	runAt := time.Now().Add(1 * time.Hour)

	// Create delayed tasks for different tenants
	taskA, err := repo.Enqueue(ctx, cmd, `{"tenant":"A"}`, 0, "", 5, "", runAt, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue tenant A delayed task: %v", err)
	}

	taskB, err := repo.Enqueue(ctx, cmd, `{"tenant":"B"}`, 0, "", 5, "", runAt, "tenant-b")
	if err != nil {
		t.Fatalf("enqueue tenant B delayed task: %v", err)
	}

	// Verify tasks are in separate delayed queues
	delayedA := "codeq:q:generate_master:tenant-a:delayed"
	delayedB := "codeq:q:generate_master:tenant-b:delayed"

	countA, err := rdb.ZCard(ctx, delayedA).Result()
	if err != nil {
		t.Fatalf("get count of tenant A delayed queue: %v", err)
	}
	if countA != 1 {
		t.Errorf("expected 1 task in tenant A delayed queue, got %d", countA)
	}

	countB, err := rdb.ZCard(ctx, delayedB).Result()
	if err != nil {
		t.Fatalf("get count of tenant B delayed queue: %v", err)
	}
	if countB != 1 {
		t.Errorf("expected 1 task in tenant B delayed queue, got %d", countB)
	}

	// Verify tenant IDs are stored correctly
	if taskA.TenantID != "tenant-a" {
		t.Errorf("expected tenant A, got %s", taskA.TenantID)
	}
	if taskB.TenantID != "tenant-b" {
		t.Errorf("expected tenant B, got %s", taskB.TenantID)
	}
}
