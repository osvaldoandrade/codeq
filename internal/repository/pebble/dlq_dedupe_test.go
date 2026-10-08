package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Dead-letter operations (ADR 0009) against deduplication keys (ADR 0004).

const dlqDedupeKey = "dedupe-k"

// enqueueDeduplicated creates a task of dlqCmd with deduplication key
// dlqDedupeKey; it returns the task the create wrote or joined.
func enqueueDeduplicated(t *testing.T, repo *TaskRepository, maxAttempts int, visibleAt time.Time) *domain.Task {
	t.Helper()
	task, err := repo.Enqueue(context.Background(), dlqCmd, `{}`, 0, "", maxAttempts, "", dlqDedupeKey, visibleAt, dlqTenant)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return task
}

func TestDeleteOfAWaitingTaskReleasesItsDeduplicationKey(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		visibleAt time.Time
	}{
		{"ready task", time.Time{}},
		{"delayed task", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, db := newDLQRepo(t)
			first := enqueueDeduplicated(t, repo, 3, tc.visibleAt)
			mapping := KeyDedupe(dlqCmd, dlqTenant, dlqDedupeKey)
			if !has(t, db, mapping) {
				t.Fatalf("create did not write the deduplication mapping")
			}
			if err := repo.DeleteTask(ctx, first.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if has(t, db, mapping) {
				t.Fatalf("delete left the deduplication mapping behind")
			}
			next := enqueueDeduplicated(t, repo, 3, time.Time{})
			if next.ID == first.ID || next.Status != domain.StatusPending {
				t.Fatalf("create after delete = %s (%s); want a new pending task", next.ID, next.Status)
			}
			if again := enqueueDeduplicated(t, repo, 3, time.Time{}); again.ID != next.ID {
				t.Fatalf("create while the new task waits = %s; want it to join %s", again.ID, next.ID)
			}
		})
	}
}

// A requeued task retries; it does not take its deduplication key back, and
// neither its next claim nor its delete may drop the key of a newer task
// that holds it meanwhile.
func TestRequeuedTaskLeavesTheDeduplicationKeyOfANewerTask(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	mapping := KeyDedupe(dlqCmd, dlqTenant, dlqDedupeKey)

	old := enqueueDeduplicated(t, repo, 1, time.Time{})
	if claimed, ok := claimOne(t, repo); !ok || claimed.ID != old.ID {
		t.Fatalf("claim = %v, %v; want %s", claimed, ok, old.ID)
	}
	// The claim released the key; a newer task takes it and waits (delayed,
	// so the next claim reaches the requeued task instead).
	newer := enqueueDeduplicated(t, repo, 3, time.Now().Add(time.Hour))
	if newer.ID == old.ID {
		t.Fatalf("create after the claim joined the claimed task")
	}
	if _, dlq, err := repo.Nack(ctx, old.ID, dlqWorker, 0, 1, dlqReason); err != nil || !dlq {
		t.Fatalf("nack: dlq=%v err=%v", dlq, err)
	}

	assertHolder := func(step string) {
		t.Helper()
		holder, err := db.Get(mapping)
		if err != nil || string(holder) != newer.ID {
			t.Fatalf("%s: mapping = %q, %v; want %s", step, holder, err, newer.ID)
		}
		if joined := enqueueDeduplicated(t, repo, 3, time.Time{}); joined.ID != newer.ID {
			t.Fatalf("%s: create = %s; want it to join %s", step, joined.ID, newer.ID)
		}
	}

	if _, err := repo.RequeueDLQTask(ctx, old.ID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	assertHolder("after requeue")

	if claimed, ok := claimOne(t, repo); !ok || claimed.ID != old.ID {
		t.Fatalf("claim of the requeued task = %v, %v; want %s", claimed, ok, old.ID)
	}
	assertHolder("after claiming the requeued task")

	if _, dlq, err := repo.Nack(ctx, old.ID, dlqWorker, 0, 1, dlqReason); err != nil || !dlq {
		t.Fatalf("second nack: dlq=%v err=%v", dlq, err)
	}
	if _, err := repo.RequeueDLQTask(ctx, old.ID); err != nil {
		t.Fatalf("second requeue: %v", err)
	}
	if err := repo.DeleteTask(ctx, old.ID); err != nil {
		t.Fatalf("delete of the requeued task: %v", err)
	}
	assertHolder("after deleting the requeued task")
}
