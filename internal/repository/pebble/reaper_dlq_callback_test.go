package pebble

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TestReaperDLQCallbackOnLeaseExpired verifies that when sweepLeases
// requeues a task whose attempts are already at the limit, the configured
// DLQCallback fires with a synthetic FAILED/LEASE_EXPIRED ResultRecord.
//
// Webhook coverage gap fix: pre-change the reaper silently moved tasks to
// DLQ without notifying anyone. Producers that registered task.Webhook
// got nothing when their work was abandoned.
func TestReaperDLQCallbackOnLeaseExpired(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 0, "https://hook.example/z", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 30, 1, 1, "")
	if err != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim: ok=%v err=%v claimed=%+v", ok, err, claimed)
	}

	db.Leases.Set(task.ID, "w1", cmd, "", 1) // unix=1 → far past

	var (
		mu    sync.Mutex
		calls []domain.ResultRecord
		tasks []domain.Task
	)
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{
		LeaseInterval:      time.Hour,
		MaxAttemptsDefault: 1,
		DLQCallback: func(ctx context.Context, t domain.Task, rec domain.ResultRecord) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, rec)
			tasks = append(tasks, t)
		},
	})

	n, err := r.sweepLeases(ctx)
	if err != nil {
		t.Fatalf("sweepLeases: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reaped task, got %d", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 DLQCallback call, got %d", len(calls))
	}
	if calls[0].Status != domain.StatusFailed {
		t.Errorf("rec.Status: want FAILED, got %s", calls[0].Status)
	}
	if calls[0].Error != "LEASE_EXPIRED" {
		t.Errorf("rec.Error: want LEASE_EXPIRED, got %q", calls[0].Error)
	}
	if tasks[0].ID != task.ID {
		t.Errorf("task.ID mismatch: want %s, got %s", task.ID, tasks[0].ID)
	}
	if tasks[0].LastKnownLocation != domain.LocationDLQ {
		t.Errorf("task.LastKnownLocation: want DLQ, got %s", tasks[0].LastKnownLocation)
	}
}

// TestReaperDLQCallbackOnlyOnTerminal verifies that the callback does
// NOT fire when the reaper merely requeues (attempts not yet exhausted).
func TestReaperDLQCallbackOnlyOnTerminal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 0, "https://hook.example/q", 5, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, _, _ = repo.Claim(ctx, "w1", []domain.Command{cmd}, 30, 1, 5, "")
	db.Leases.Set(task.ID, "w1", cmd, "", 1)

	called := 0
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{
		LeaseInterval:      time.Hour,
		MaxAttemptsDefault: 5,
		DLQCallback: func(ctx context.Context, t domain.Task, rec domain.ResultRecord) {
			called++
		},
	})
	if _, err := r.sweepLeases(ctx); err != nil {
		t.Fatalf("sweepLeases: %v", err)
	}
	if called != 0 {
		t.Fatalf("expected 0 callback calls on requeue, got %d", called)
	}
}
