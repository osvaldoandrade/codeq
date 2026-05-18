package services

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

// captureCallback records every Send invocation so tests can assert
// webhook coverage across terminal paths (Submit, BatchSubmit, Nack→DLQ,
// reaper→DLQ).
type captureCallback struct {
	mu    sync.Mutex
	calls []captureEntry
}

type captureEntry struct {
	task domain.Task
	rec  domain.ResultRecord
}

func (c *captureCallback) Send(ctx context.Context, task domain.Task, rec domain.ResultRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, captureEntry{task: task, rec: rec})
}

func (c *captureCallback) snapshot() []captureEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]captureEntry, len(c.calls))
	copy(out, c.calls)
	return out
}

func TestBatchSubmitFiresCallbackPerItem(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	t1, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"k":1}`, 0, "https://hook.example/1", 5, "", time.Time{}, "")
	t2, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"k":2}`, 0, "https://hook.example/2", 5, "", time.Time{}, "")
	cmds := []domain.Command{domain.CmdGenerateMaster}
	_, _, _ = taskRepo.Claim(context.Background(), "w1", cmds, 30, 1, 5, "")
	_, _, _ = taskRepo.Claim(context.Background(), "w1", cmds, 30, 1, 5, "")

	resultRepo := repository.NewResultRepository(rdb, time.UTC, nil)
	cb := &captureCallback{}
	svc := NewResultsService(resultRepo, &mockResultsUploader{}, cb, slog.Default(), time.Now, time.UTC)

	items := []domain.BatchSubmitItem{
		{TaskID: t1.ID, SubmitResultRequest: domain.SubmitResultRequest{WorkerID: "w1", Status: domain.StatusCompleted, Result: map[string]any{"ok": true}}},
		{TaskID: t2.ID, SubmitResultRequest: domain.SubmitResultRequest{WorkerID: "w1", Status: domain.StatusFailed, Error: "boom"}},
	}
	if _, err := svc.BatchSubmit(context.Background(), items); err != nil {
		t.Fatalf("BatchSubmit: %v", err)
	}

	got := cb.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 callback calls, got %d", len(got))
	}
	seen := map[string]domain.TaskStatus{}
	for _, e := range got {
		seen[e.task.ID] = e.rec.Status
	}
	if seen[t1.ID] != domain.StatusCompleted {
		t.Errorf("t1 status: want COMPLETED, got %s", seen[t1.ID])
	}
	if seen[t2.ID] != domain.StatusFailed {
		t.Errorf("t2 status: want FAILED, got %s", seen[t2.ID])
	}
}

func TestNackTerminalFiresCallback(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	mockSubRepo := &mockSubscriptionRepo{}
	notifier := NewNotifierService(mockSubRepo, nil, "test-secret", 5, nil, ratelimit.Bucket{}, nil)
	cb := &captureCallback{}
	// maxAttemptsDefault=1 so the first Nack lands the task in DLQ.
	svc := NewSchedulerService(taskRepo, notifier, cb, time.UTC, time.Now, 60, 50, 1, "exp_full_jitter", 5, 900)

	created, err := svc.CreateTask(context.Background(), domain.CmdGenerateMaster, `{"k":1}`, 0, "https://hook.example/x", 1, "", time.Time{}, 0, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	claimed, ok, err := svc.ClaimTask(context.Background(), "w1", []domain.Command{domain.CmdGenerateMaster}, 30, 0, "")
	if err != nil || !ok {
		t.Fatalf("ClaimTask: ok=%v err=%v", ok, err)
	}
	if claimed.ID != created.ID {
		t.Fatalf("claimed wrong task: %s != %s", claimed.ID, created.ID)
	}
	_, terminal, err := svc.NackTask(context.Background(), claimed.ID, "w1", 0, "fatal")
	if err != nil {
		t.Fatalf("NackTask: %v", err)
	}
	if !terminal {
		t.Fatalf("expected terminal=true with maxAttempts=1")
	}
	got := cb.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 callback call, got %d", len(got))
	}
	if got[0].rec.Status != domain.StatusFailed {
		t.Errorf("status: want FAILED, got %s", got[0].rec.Status)
	}
	if got[0].rec.Error != "fatal" {
		t.Errorf("error: want fatal, got %q", got[0].rec.Error)
	}
	if got[0].task.ID != created.ID {
		t.Errorf("task.ID mismatch: want %s, got %s", created.ID, got[0].task.ID)
	}
}

func TestNackNonTerminalDoesNotFireCallback(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	mockSubRepo := &mockSubscriptionRepo{}
	notifier := NewNotifierService(mockSubRepo, nil, "test-secret", 5, nil, ratelimit.Bucket{}, nil)
	cb := &captureCallback{}
	// maxAttempts=5; first Nack should retry, not DLQ.
	svc := NewSchedulerService(taskRepo, notifier, cb, time.UTC, time.Now, 60, 50, 5, "exp_full_jitter", 5, 900)

	created, _ := svc.CreateTask(context.Background(), domain.CmdGenerateMaster, `{"k":1}`, 0, "https://hook.example/y", 5, "", time.Time{}, 0, "")
	_, _, _ = svc.ClaimTask(context.Background(), "w1", []domain.Command{domain.CmdGenerateMaster}, 30, 0, "")
	_, terminal, _ := svc.NackTask(context.Background(), created.ID, "w1", 1, "retry-me")
	if terminal {
		t.Fatalf("expected terminal=false on first Nack with maxAttempts=5")
	}
	if got := cb.snapshot(); len(got) != 0 {
		t.Fatalf("expected 0 callback calls on retry, got %d", len(got))
	}
}
