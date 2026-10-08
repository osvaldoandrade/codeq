package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const dlqServiceTenant = "tenant-a"

// readyRecorder counts NotifyQueueReady calls per command.
type readyRecorder struct{ calls map[domain.Command]int }

func (r *readyRecorder) NotifyQueueReady(_ context.Context, cmd domain.Command) { r.calls[cmd]++ }

func newDLQService(t *testing.T) (context.Context, SchedulerService, *readyRecorder) {
	t.Helper()
	notifier := &readyRecorder{calls: map[domain.Command]int{}}
	now := func() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) }
	svc := NewSchedulerService(openPebbleStores(t).tasks, notifier, nil, time.UTC, now, 60, 50, 1, "fixed", 1, 900)
	return context.Background(), svc, notifier
}

// deadLetterViaService creates a one-attempt task, claims it and nacks it.
func deadLetterViaService(t *testing.T, ctx context.Context, svc SchedulerService) *domain.Task {
	t.Helper()
	task, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{}`, 5, "", 1, "", "", time.Time{}, 0, dlqServiceTenant)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok, err := svc.ClaimTask(ctx, "w", []domain.Command{domain.CmdGenerateMaster}, 60, 0, dlqServiceTenant); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, dlq, err := svc.NackTask(ctx, task.ID, "w", 0, "boom"); err != nil || !dlq {
		t.Fatalf("nack: %v %v", dlq, err)
	}
	return task
}

func TestRequeueDLQTaskNotifiesWorkers(t *testing.T) {
	ctx, svc, notifier := newDLQService(t)
	task := deadLetterViaService(t, ctx, svc)
	before := notifier.calls[domain.CmdGenerateMaster]

	got, err := svc.RequeueDLQTask(ctx, task.ID)
	if err != nil || got.Status != domain.StatusPending {
		t.Fatalf("requeue = %+v, %v", got, err)
	}
	if notifier.calls[domain.CmdGenerateMaster] != before+1 {
		t.Fatal("requeue did not wake the workers of the command")
	}
	if _, err := svc.RequeueDLQTask(ctx, task.ID); !errors.Is(err, domain.ErrTaskNotInDLQ) {
		t.Fatalf("second requeue err %v", err)
	}
	if notifier.calls[domain.CmdGenerateMaster] != before+1 {
		t.Fatal("a refused requeue notified")
	}
}

func TestRequeueDLQValidatesDefaultsAndNotifies(t *testing.T) {
	ctx, svc, notifier := newDLQService(t)
	for _, limit := range []int{-1, MaxDLQRequeueLimit + 1} {
		if _, err := svc.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqServiceTenant, limit); !errors.Is(err, domain.ErrInvalidRequeueLimit) {
			t.Fatalf("limit %d: err %v, want ErrInvalidRequeueLimit", limit, err)
		}
	}
	if _, err := svc.RequeueDLQ(ctx, " ", dlqServiceTenant, 10); err == nil {
		t.Fatal("blank command: want an error")
	}
	res, err := svc.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqServiceTenant, 0)
	if err != nil || res.Requeued != 0 || res.Remaining || notifier.calls[domain.CmdGenerateMaster] != 0 {
		t.Fatalf("empty queue: %+v, %v, %d notifications", res, err, notifier.calls[domain.CmdGenerateMaster])
	}

	deadLetterViaService(t, ctx, svc)
	before := notifier.calls[domain.CmdGenerateMaster]
	res, err = svc.RequeueDLQ(ctx, domain.CmdGenerateMaster, dlqServiceTenant, 0)
	if err != nil || res.Requeued != 1 || res.Remaining {
		t.Fatalf("default limit: %+v, %v", res, err)
	}
	if notifier.calls[domain.CmdGenerateMaster] != before+1 {
		t.Fatal("bulk requeue did not wake the workers")
	}
}

func TestDeleteTaskPassesThrough(t *testing.T) {
	ctx, svc, _ := newDLQService(t)
	task := deadLetterViaService(t, ctx, svc)
	if err := svc.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetTask(ctx, task.ID); err == nil {
		t.Fatal("task readable after delete")
	}
}
