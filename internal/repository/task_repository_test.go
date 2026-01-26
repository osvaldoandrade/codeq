package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func setupRepo(t *testing.T) (context.Context, *miniredis.Miniredis, *redis.Client, TaskRepository) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	return context.Background(), mr, rdb, repo
}

func TestEnqueueIdempotent(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task1, err := repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-123")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	task2, err := repo.Enqueue(ctx, cmd, `{"a":1}`, 0, "", 5, "job-123")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if task1.ID != task2.ID {
		t.Fatalf("expected same task id for idempotency key, got %s vs %s", task1.ID, task2.ID)
	}
	key := "codeq:q:" + strings.ToLower(string(cmd)) + ":pending:0"
	if n, _ := rdb.LLen(ctx, key).Result(); n != 1 {
		t.Fatalf("expected 1 pending item, got %d", n)
	}
}

func TestPriorityClaim(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	low, err := repo.Enqueue(ctx, cmd, `{"p":0}`, 0, "", 5, "")
	if err != nil {
		t.Fatalf("enqueue low: %v", err)
	}
	high, err := repo.Enqueue(ctx, cmd, `{"p":9}`, 9, "", 5, "")
	if err != nil {
		t.Fatalf("enqueue high: %v", err)
	}
	got, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 5)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatalf("expected claim to succeed")
	}
	if got.ID != high.ID {
		t.Fatalf("expected high priority task, got %s (low=%s)", got.ID, low.ID)
	}
}

func TestNackDelayedAndDLQ(t *testing.T) {
	ctx, _, rdb, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 3, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 3)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("expected attempts=1 after claim, got %d", claimed.Attempts)
	}

	delay, dlq, err := repo.Nack(ctx, task.ID, "worker-1", 0, 3, "ERR")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if dlq {
		t.Fatalf("expected not in dlq on first nack")
	}
	if delay != 0 {
		t.Fatalf("expected delay 0, got %d", delay)
	}

	if moved, err := repo.MoveDueDelayed(ctx, cmd, 10); err != nil || moved != 1 {
		t.Fatalf("move due delayed: moved=%d err=%v", moved, err)
	}

	claimed2, ok, err := repo.Claim(ctx, "worker-1", []domain.Command{cmd}, 60, 50, 3)
	if err != nil || !ok {
		t.Fatalf("claim 2: ok=%v err=%v", ok, err)
	}
	if claimed2.Attempts != 3 {
		t.Fatalf("expected attempts=3 after second claim, got %d", claimed2.Attempts)
	}

	_, dlq, err = repo.Nack(ctx, task.ID, "worker-1", 0, 3, "ERR")
	if err != nil {
		t.Fatalf("nack 2: %v", err)
	}
	if !dlq {
		t.Fatalf("expected dlq on second nack")
	}
	stored, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("expected failed status, got %s", stored.Status)
	}
	dlqKey := "codeq:q:" + strings.ToLower(string(cmd)) + ":dlq"
	if n, _ := rdb.LLen(ctx, dlqKey).Result(); n != 1 {
		t.Fatalf("expected 1 item in dlq, got %d", n)
	}
}

func TestCleanupExpired(t *testing.T) {
	ctx, _, _, repo := setupRepo(t)
	cmd := domain.CmdGenerateMaster
	task1, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 0, "", 5, "")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	task2, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 0, "", 5, "")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	deleted, err := repo.CleanupExpired(ctx, 10, time.Now().Add(25*time.Hour))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("expected at least 2 deletions, got %d", deleted)
	}
	if _, err := repo.Get(ctx, task1.ID); err == nil {
		t.Fatalf("expected task1 to be deleted")
	}
	if _, err := repo.Get(ctx, task2.ID); err == nil {
		t.Fatalf("expected task2 to be deleted")
	}
}
