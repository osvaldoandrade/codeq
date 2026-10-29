package pebble

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// assertNoTrace fails if any key or value in the store still mentions id.
func assertNoTrace(t *testing.T, db *DB, id string) {
	t.Helper()
	it, err := db.Iter([]byte(namespace), prefixUpper([]byte(namespace)))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	for valid := it.First(); valid; valid = it.Next() {
		if bytes.Contains(it.Key(), []byte(id)) || bytes.Contains(it.Value(), []byte(id)) {
			t.Errorf("retained %q", it.Key())
		}
	}
	if err = it.Error(); err != nil {
		t.Fatal(err)
	}
}

func completeAbsolute(t *testing.T, repo *TaskRepository, results *ResultRepository, id string, status domain.TaskStatus) {
	t.Helper()
	ctx := context.Background()
	claimed, ok, err := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || claimed.ID != id {
		t.Fatalf("claim=%+v,%v,%v", claimed, ok, err)
	}
	if err = results.SaveResult(ctx, domain.ResultRecord{TaskID: id, Status: status, Result: map[string]any{"outcome": "private"}}, absoluteTestTopic, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err = results.UpdateTaskOnComplete(ctx, id, absoluteTestTopic, "tenant-a", status, "failed"); err != nil {
		t.Fatal(err)
	}
}

// An acknowledged absolute task is removed entirely at once: payload, result,
// lease, every queue/TTL/dedupe reference and the identity, with no replay
// tombstone, so the store keeps no trace of the ID and the budget is released.
func TestAbsoluteCompletionLeavesNothing(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "logs-acknowledged", "", time.Time{}, 100)
	if n, err := db.absoluteCount(); err != nil || n != 1 {
		t.Fatalf("count before = %d, %v", n, err)
	}
	completeAbsolute(t, repo, results, task.ID, domain.StatusCompleted)

	if got, err := repo.Get(ctx, task.ID); err == nil || got != nil {
		t.Fatalf("acknowledged task still readable: %+v %v", got, err)
	}
	if got, err := results.GetResult(ctx, task.ID); err == nil || got != nil {
		t.Fatalf("acknowledged result still readable: %+v %v", got, err)
	}
	assertNoPayloadIndexes(t, db, task.ID)
	if identity, err := db.readAbsoluteIdentity(task.ID); err != nil || identity != nil {
		t.Fatalf("identity retained: %+v %v", identity, err)
	}
	if n, err := db.absoluteCount(); err != nil || n != 0 {
		t.Fatalf("count after = %d, %v", n, err)
	}
	assertNoTrace(t, db, task.ID)

	// The consumer owns replay protection: publishing the ID again is a new task.
	again := enqueueAbsolute(t, repo, clock, task.ID, "", time.Time{}, 100)
	if again.ID != task.ID || again.Status != domain.StatusPending {
		t.Fatalf("republished task = %+v", again)
	}
	if n, err := db.absoluteCount(); err != nil || n != 1 {
		t.Fatalf("count after republish = %d, %v", n, err)
	}
}

// A terminal failure is not an acknowledgement: the task keeps its custody
// (and later its bounded tombstone) exactly as before.
func TestAbsoluteFailureKeepsCustody(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	task := enqueueAbsolute(t, repo, clock, "logs-failed-result", "", time.Time{}, 100)
	completeAbsolute(t, repo, results, task.ID, domain.StatusFailed)
	if yes, err := db.Has(KeyTask(task.ID)); err != nil || !yes {
		t.Fatalf("failed task body dropped before its deadline: %v %v", yes, err)
	}
	if identity, err := db.readAbsoluteIdentity(task.ID); err != nil || identity == nil || identity.RetiredAt != nil {
		t.Fatalf("failed task identity = %+v %v", identity, err)
	}
}
