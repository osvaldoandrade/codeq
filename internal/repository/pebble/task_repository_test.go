package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnqueueClaimComplete(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	enq, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if enq.Status != domain.StatusPending {
		t.Fatalf("expected status PENDING, got %s", enq.Status)
	}

	got, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if got.ID != enq.ID {
		t.Fatalf("expected %s, got %s", enq.ID, got.ID)
	}
	if got.Status != domain.StatusInProgress {
		t.Fatalf("expected INPROG, got %s", got.Status)
	}
	if got.WorkerID != "w1" {
		t.Fatalf("expected worker w1, got %s", got.WorkerID)
	}

	if err := repo.Heartbeat(ctx, got.ID, "w1", 120); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func TestPriorityOrder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	low, _ := repo.Enqueue(ctx, cmd, `{"p":0}`, 0, "", 3, "", time.Time{}, "")
	high, _ := repo.Enqueue(ctx, cmd, `{"p":9}`, 9, "", 3, "", time.Time{}, "")
	mid, _ := repo.Enqueue(ctx, cmd, `{"p":5}`, 5, "", 3, "", time.Time{}, "")

	for i, want := range []*domain.Task{high, mid, low} {
		got, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		if got.ID != want.ID {
			t.Fatalf("claim %d: expected %s, got %s", i, want.ID, got.ID)
		}
	}
}

func TestFIFOWithinPriority(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	a, _ := repo.Enqueue(ctx, cmd, `{"o":"a"}`, 5, "", 3, "", time.Time{}, "")
	b, _ := repo.Enqueue(ctx, cmd, `{"o":"b"}`, 5, "", 3, "", time.Time{}, "")
	c, _ := repo.Enqueue(ctx, cmd, `{"o":"c"}`, 5, "", 3, "", time.Time{}, "")

	for i, want := range []*domain.Task{a, b, c} {
		got, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		if got.ID != want.ID {
			t.Fatalf("claim %d: expected %s (%d), got %s", i, want.ID, i, got.ID)
		}
	}
}

func TestNackDelayedThenReclaim(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	// Make reconcile eager — the test would otherwise wait 500ms.
	repo.reconcile.interval = 0
	cmd := domain.CmdGenerateMaster

	enq, _ := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	claimed, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	delay, dlq, err := repo.Nack(ctx, claimed.ID, "w", 0, 3, "EPHEMERAL")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if dlq {
		t.Fatal("did not expect DLQ on first nack")
	}
	if delay != 0 {
		t.Fatalf("expected delay=0, got %d", delay)
	}

	// MoveDueDelayed surfaces the just-nacked task immediately (visibleAt=now).
	again, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	if again.ID != enq.ID {
		t.Fatalf("expected same id back, got %s vs %s", again.ID, enq.ID)
	}
}

func TestNackEventuallyDLQ(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 1)
	repo.reconcile.interval = 0
	cmd := domain.CmdGenerateMaster

	// Each Claim and each Nack increment attempts. With maxAttempts=3 the
	// sequence is: claim(1)→nack(2,<3)→delayed → claim(3)→nack(4,>=3)→DLQ.
	_, _ = repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	for i := 0; i < 2; i++ {
		c, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if c == nil {
			t.Fatalf("claim %d: no task", i)
		}
		dly, dlq, err := repo.Nack(ctx, c.ID, "w", 0, 3, "ERR")
		if err != nil {
			t.Fatalf("nack %d: %v", i, err)
		}
		if i == 1 {
			if !dlq {
				t.Fatalf("expected DLQ on second nack cycle, got delay=%d", dly)
			}
		}
	}
}

func TestIdempotencyReturnsOriginal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	a, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "job-7", time.Time{}, "")
	if err != nil {
		t.Fatalf("enq1: %v", err)
	}
	b, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 5, "", 3, "job-7", time.Time{}, "")
	if err != nil {
		t.Fatalf("enq2: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("expected same id for idempotency key, got %s vs %s", a.ID, b.ID)
	}
}
