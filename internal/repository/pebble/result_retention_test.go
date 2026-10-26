package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// completeNamed creates, claims and completes a legacy named task with a
// stored result, the sequence the HTTP /result route performs.
func completeNamed(t *testing.T, tasks *TaskRepository, results *ResultRepository, id, payload string) {
	t.Helper()
	ctx := context.Background()
	if _, err := tasks.Enqueue(ctx, domain.CmdGenerateMaster, payload, 5, "", 3, "", "", id, time.Time{}, namedTenant); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := tasks.Claim(ctx, "w", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, namedTenant)
	if err != nil || !ok || claimed.ID != id {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	rec := domain.ResultRecord{TaskID: id, Status: domain.StatusCompleted, Result: map[string]any{"from": payload}, CompletedAt: time.Now()}
	if err = results.SaveResult(ctx, rec, domain.CmdGenerateMaster, namedTenant); err != nil {
		t.Fatal(err)
	}
	if err = results.UpdateTaskOnComplete(ctx, id, domain.CmdGenerateMaster, namedTenant, domain.StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionReapsLegacyResultWithTerminalBody(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	tasks := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	results := NewResultRepository(db, time.UTC)
	completeNamed(t, tasks, results, namedID, "first")

	if n, err := tasks.CleanupExpired(ctx, 100, time.Now().Add(taskRetention+time.Hour)); err != nil || n != 1 {
		t.Fatalf("cleanup = %d, %v", n, err)
	}
	for _, key := range [][]byte{KeyTask(namedID), KeyResult(namedID)} {
		if yes, err := db.Has(key); err != nil || yes {
			t.Fatalf("retained %q after retention: %v %v", key, yes, err)
		}
	}

	// ADR-0008: the reaped ID is reusable and must not inherit the old result.
	if _, err := tasks.Enqueue(ctx, domain.CmdGenerateMaster, `{"n":2}`, 5, "", 3, "", "", namedID, time.Time{}, namedTenant); err != nil {
		t.Fatal(err)
	}
	if rec, err := results.GetResult(ctx, namedID); err == nil || rec != nil {
		t.Fatalf("reused id exposes the previous result: %+v, %v", rec, err)
	}
}

func TestRetentionKeepsResultOfLiveBody(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	tasks := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	results := NewResultRepository(db, time.UTC)
	completeNamed(t, tasks, results, namedID, "first")

	if _, err := tasks.CleanupExpired(ctx, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec, err := results.GetResult(ctx, namedID); err != nil || rec == nil || rec.TaskID != namedID {
		t.Fatalf("result of a live task = %+v, %v", rec, err)
	}
}
