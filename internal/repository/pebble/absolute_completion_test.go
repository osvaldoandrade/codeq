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

// A duplicate acknowledgement finds nothing to retire, and one that arrives
// after the ID was published again never purges the new incarnation.
func TestAbsoluteCompletionRefusesStaleAcknowledgements(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	ctx := context.Background()
	first := enqueueAbsolute(t, repo, clock, "logs-stale-ack", "", time.Time{}, 100)
	completeAbsolute(t, repo, results, first.ID, domain.StatusCompleted)
	if err := results.UpdateTaskOnComplete(ctx, first.ID, absoluteTestTopic, "tenant-a", domain.StatusCompleted, ""); err == nil {
		t.Fatal("duplicate acknowledgement of a retired task succeeded")
	}
	clock.Add(int64(time.Millisecond))
	again := enqueueAbsolute(t, repo, clock, first.ID, "", time.Time{}, 100)
	if err := db.retireCompletedAbsolute(first); err == nil || err.Error() != "not-in-progress" {
		t.Fatalf("stale acknowledgement against the re-published ID = %v", err)
	}
	if got, err := repo.Get(ctx, again.ID); err != nil || got == nil || !got.CreatedAt.Equal(again.CreatedAt) {
		t.Fatalf("re-published task lost: %+v %v", got, err)
	}
}

// An acknowledgement racing the deadline leaves retirement to the sweep and
// its bounded tombstone.
func TestAbsoluteCompletionAfterDeadlineIsRefused(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "logs-late-ack", "", time.Time{}, 100)
	claimed, ok, err := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim=%+v,%v,%v", claimed, ok, err)
	}
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	if err = db.retireCompletedAbsolute(claimed); err == nil {
		t.Fatal("acknowledgement after the deadline retired the task")
	}
	if identity, e := db.readAbsoluteIdentity(task.ID); e != nil || identity == nil {
		t.Fatalf("identity lost before the sweep: %+v %v", identity, e)
	}
}

// A restored in-progress row without an identity manifest is retired on
// acknowledgement without touching the identity count.
func TestAbsoluteCompletionOfRestoredRowKeepsCount(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "logs-restored", "", time.Time{}, 100)
	claimed, ok, err := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim=%+v,%v,%v", claimed, ok, err)
	}
	// Simulate a row restored without its manifest.
	b := db.Batch()
	if err = db.stageAbsoluteIdentityRemoval(b, newAbsoluteIdentity(task)); err != nil {
		t.Fatal(err)
	}
	if err = db.CommitBatch(b); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if n, e := db.absoluteCount(); e != nil || n != 0 {
		t.Fatalf("count before = %d, %v", n, e)
	}
	if err = results.UpdateTaskOnComplete(ctx, task.ID, absoluteTestTopic, "tenant-a", domain.StatusCompleted, ""); err != nil {
		t.Fatal(err)
	}
	assertNoTrace(t, db, task.ID)
	if n, e := db.absoluteCount(); e != nil || n != 0 {
		t.Fatalf("count after = %d, %v", n, e)
	}
}

// The batch acknowledgement path (HTTP batch and gRPC streams) retires
// acknowledged absolute tasks too, and leaves other completions unchanged.
func TestAbsoluteBatchCompletionLeavesNothing(t *testing.T) {
	db, repo, results, clock := absoluteFixture(t)
	ctx := context.Background()
	task := enqueueAbsolute(t, repo, clock, "logs-batch-ack", "", time.Time{}, 100)
	claimed, ok, err := repo.Claim(ctx, absoluteTestWorker, []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
	if err != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim=%+v,%v,%v", claimed, ok, err)
	}
	legacy, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{"n":1}`, 0, "", 3, "", "", "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, e := repo.Claim(ctx, "w", []domain.Command{domain.CmdGenerateMaster}, 60, 100, 3, "tenant-a"); e != nil || !ok || got.ID != legacy.ID {
		t.Fatalf("legacy claim=%+v,%v,%v", got, ok, e)
	}
	updates := []domain.TaskCompleteUpdate{
		{ID: task.ID, Status: domain.StatusCompleted},
		{ID: legacy.ID, Status: domain.StatusCompleted},
	}
	if err = results.BatchUpdateTasksOnComplete(ctx, updates); err != nil {
		t.Fatal(err)
	}
	assertNoTrace(t, db, task.ID)
	if got, e := repo.Get(ctx, legacy.ID); e != nil || got == nil || got.Status != domain.StatusCompleted {
		t.Fatalf("legacy completion changed: %+v %v", got, e)
	}
}
