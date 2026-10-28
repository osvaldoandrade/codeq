package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// staleDelayedFixture is one claimed task plus the reaper that requeues it.
type staleDelayedFixture struct {
	db     *DB
	repo   *TaskRepository
	reaper *Reaper
	cmd    domain.Command
	id     string
}

func newStaleDelayedFixture(t *testing.T) staleDelayedFixture {
	t.Helper()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	reaper := NewReaper(db, time.UTC, nil, ReaperOptions{MaxAttemptsDefault: 10, OnDelayed: repo.NoteDelayed})
	cmd := domain.CmdGenerateMaster
	task, err := repo.Enqueue(context.Background(), cmd, `{"k":1}`, 0, "", 10, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return staleDelayedFixture{db: db, repo: repo, reaper: reaper, cmd: cmd, id: task.ID}
}

func (f staleDelayedFixture) claim(t *testing.T, worker string) {
	t.Helper()
	got, ok, err := f.repo.Claim(context.Background(), worker, []domain.Command{f.cmd}, 60, 50, 10, "")
	if err != nil || !ok || got.ID != f.id {
		t.Fatalf("claim by %s: ok=%v err=%v", worker, ok, err)
	}
}

func (f staleDelayedFixture) body(t *testing.T) domain.Task {
	t.Helper()
	raw, err := f.db.Get(KeyTask(f.id))
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	var task domain.Task
	if err := sonic.Unmarshal(raw, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return task
}

func (f staleDelayedFixture) move(t *testing.T) {
	t.Helper()
	if _, err := f.repo.MoveDueDelayed(context.Background(), f.cmd, 50); err != nil {
		t.Fatalf("move due delayed: %v", err)
	}
}

func (f staleDelayedFixture) count(t *testing.T, lower, upper []byte) int {
	t.Helper()
	it, err := f.db.Iter(lower, upper)
	if err != nil {
		t.Fatalf("iter: %v", err)
	}
	defer it.Close()
	n := 0
	for valid := it.First(); valid; valid = it.Next() {
		n++
	}
	return n
}

// injectDueDelayedKey writes the leftover key of a second requeue that is
// already due, as the losing lease-expiry repair leaves it.
func (f staleDelayedFixture) injectDueDelayedKey(t *testing.T) {
	t.Helper()
	b := f.db.Batch()
	defer b.Close()
	if err := b.Set(KeyDelayed(f.cmd, "", unixSeconds(time.Now())-1, f.id), nil, nil); err != nil {
		t.Fatalf("stage delayed key: %v", err)
	}
	if err := f.db.CommitBatch(b); err != nil {
		t.Fatalf("commit delayed key: %v", err)
	}
	f.repo.NoteDelayed(f.cmd, "")
}

// TestMoveDueDelayedKeepsReclaimedTaskAfterDuplicateRequeue reproduces the
// production not-owner refusal: two lease-expiry repairs both read the
// task IN_PROGRESS and each write a delayed key with its own score. The
// first key moves the task, a new worker claims it, and the second key
// comes due while that worker still processes the task.
func TestMoveDueDelayedKeepsReclaimedTaskAfterDuplicateRequeue(t *testing.T) {
	ctx := context.Background()
	f := newStaleDelayedFixture(t)
	f.claim(t, "w1")

	first := f.body(t)
	second := first // the concurrent repair read the same IN_PROGRESS body
	if err := f.reaper.requeueExpiredOne(ctx, &first, 0); err != nil {
		t.Fatalf("first requeue: %v", err)
	}
	if err := f.reaper.requeueExpiredOne(ctx, &second, 1); err != nil {
		t.Fatalf("second requeue: %v", err)
	}
	lower, upper := PrefixDelayed(f.cmd, "")
	if got := f.count(t, lower, upper); got != 2 {
		t.Fatalf("duplicate requeue left %d delayed keys, want 2", got)
	}

	f.move(t)
	f.claim(t, "w2")

	// Wait until the second key's one-second score is due.
	deadline := time.Now().Add(3 * time.Second)
	for {
		dl, du := PrefixDelayedUpTo(f.cmd, "", unixSeconds(time.Now()))
		if f.count(t, dl, du) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second delayed key never became due")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.move(t)

	got := f.body(t)
	if got.Status != domain.StatusInProgress || got.WorkerID != "w2" || got.LastKnownLocation != domain.LocationInProgress {
		t.Fatalf("stale delayed key rewrote the claimed task: status=%s worker=%q location=%s", got.Status, got.WorkerID, got.LastKnownLocation)
	}
	if err := f.repo.Heartbeat(ctx, f.id, "w2", 60); err != nil {
		t.Fatalf("owner lost its lease: %v", err)
	}
	if n := f.count(t, lower, upper); n != 0 {
		t.Fatalf("stale delayed key kept: %d", n)
	}
	pl, pu := PrefixPendingAllPrios(f.cmd, "")
	if n := f.count(t, pl, pu); n != 0 {
		t.Fatalf("claimed task gained %d pending keys", n)
	}
}

// TestMoveDueDelayedDoesNotResurrectCompletedTask covers the same stale key
// arriving after the owner completed the task.
func TestMoveDueDelayedDoesNotResurrectCompletedTask(t *testing.T) {
	ctx := context.Background()
	f := newStaleDelayedFixture(t)
	f.claim(t, "w1")
	results := NewResultRepository(f.db, time.UTC)
	if err := results.UpdateTaskOnComplete(ctx, f.id, f.cmd, "", domain.StatusCompleted, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	f.injectDueDelayedKey(t)
	f.move(t)

	got := f.body(t)
	if got.Status != domain.StatusCompleted || got.WorkerID != "" {
		t.Fatalf("completed task resurrected: status=%s worker=%q", got.Status, got.WorkerID)
	}
	if _, ok, err := f.repo.Claim(ctx, "w2", []domain.Command{f.cmd}, 60, 50, 10, ""); err != nil || ok {
		t.Fatalf("completed task claimable again: ok=%v err=%v", ok, err)
	}
	lower, upper := PrefixDelayed(f.cmd, "")
	if n := f.count(t, lower, upper); n != 0 {
		t.Fatalf("stale delayed key kept: %d", n)
	}
}

// TestMoveDueDelayedMovesTaskOnceForDuplicateDueKeys covers both keys due in
// one pass: the task moves once and both keys are removed.
func TestMoveDueDelayedMovesTaskOnceForDuplicateDueKeys(t *testing.T) {
	ctx := context.Background()
	f := newStaleDelayedFixture(t)
	f.claim(t, "w1")
	task := f.body(t)
	if err := f.reaper.requeueExpiredOne(ctx, &task, 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	f.injectDueDelayedKey(t)
	f.move(t)

	got := f.body(t)
	if got.Status != domain.StatusPending || got.LastKnownLocation != domain.LocationPending {
		t.Fatalf("delayed task not moved: status=%s location=%s", got.Status, got.LastKnownLocation)
	}
	lower, upper := PrefixDelayed(f.cmd, "")
	if n := f.count(t, lower, upper); n != 0 {
		t.Fatalf("due delayed keys kept: %d", n)
	}
	pl, pu := PrefixPendingAllPrios(f.cmd, "")
	if n := f.count(t, pl, pu); n != 1 {
		t.Fatalf("pending keys = %d, want exactly 1", n)
	}
	f.claim(t, "w2")
}

// TestMoveDueDelayedKeepsLegacyBodyWithoutLocation keeps moving a delayed
// task whose body has no recorded location.
func TestMoveDueDelayedKeepsLegacyBodyWithoutLocation(t *testing.T) {
	ctx := context.Background()
	f := newStaleDelayedFixture(t)
	f.claim(t, "w1")
	task := f.body(t)
	if err := f.reaper.requeueExpiredOne(ctx, &task, 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	legacy := f.body(t)
	legacy.LastKnownLocation = ""
	raw, _ := sonic.Marshal(&legacy)
	b := f.db.Batch()
	defer b.Close()
	if err := b.Set(KeyTask(f.id), raw, nil); err != nil {
		t.Fatalf("stage legacy body: %v", err)
	}
	if err := f.db.CommitBatch(b); err != nil {
		t.Fatalf("commit legacy body: %v", err)
	}
	f.move(t)
	if got := f.body(t); got.Status != domain.StatusPending || got.LastKnownLocation != domain.LocationPending {
		t.Fatalf("legacy delayed task not moved: status=%s location=%s", got.Status, got.LastKnownLocation)
	}
	f.claim(t, "w2")
}
