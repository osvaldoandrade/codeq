package pebble

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	dlqTenant      = "tenant-a"
	dlqOtherTenant = "tenant-b"
	dlqWorker      = "w-dlq"
	dlqReason      = "boom"
	dlqNotFound    = "not-found"
)

var dlqCmd = domain.CmdGenerateMaster

func newDLQRepo(t *testing.T) (*TaskRepository, *DB) {
	t.Helper()
	db := openTestDB(t)
	return NewTaskRepository(db, time.UTC, "fixed", 1, 5), db
}

// deadLetter enqueues a one-attempt task, claims it and nacks it, which
// moves it to the dead-letter queue. The queue must hold no other ready
// task of (cmd, tenant), so the claim takes this one.
func deadLetter(t *testing.T, repo repository.TaskRepository, cmd domain.Command, tenant string, prio int) *domain.Task {
	t.Helper()
	ctx := context.Background()
	task, err := repo.Enqueue(ctx, cmd, `{"n":1}`, prio, "", 1, "", time.Time{}, tenant)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := repo.Claim(ctx, dlqWorker, []domain.Command{cmd}, 60, 50, 1, tenant)
	if err != nil || !ok || claimed.ID != task.ID {
		t.Fatalf("claim = %v, %v, %v; want %s", claimed, ok, err, task.ID)
	}
	if _, dlq, err := repo.Nack(ctx, task.ID, dlqWorker, 0, 1, dlqReason); err != nil || !dlq {
		t.Fatalf("nack: dlq=%v err=%v", dlq, err)
	}
	return mustGet(t, repo, task.ID)
}

func enqueueTask(t *testing.T, repo repository.TaskRepository, prio int, visibleAt time.Time, tenant string) *domain.Task {
	t.Helper()
	task, err := repo.Enqueue(context.Background(), dlqCmd, `{}`, prio, "", 3, "", visibleAt, tenant)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return task
}

func mustGet(t *testing.T, repo repository.TaskRepository, id string) *domain.Task {
	t.Helper()
	task, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return task
}

func mustStats(t *testing.T, repo repository.TaskRepository) domain.QueueStats {
	t.Helper()
	st, err := repo.QueueStats(context.Background(), dlqCmd, dlqTenant)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return *st
}

// indexKeys returns every queue index key of db that points at id.
func indexKeys(t *testing.T, db *DB, id string) []string {
	t.Helper()
	lower := []byte(pQueue)
	it, err := db.Iter(lower, prefixUpper(lower))
	if err != nil {
		t.Fatalf("iter: %v", err)
	}
	defer it.Close()
	var out []string
	for valid := it.First(); valid; valid = it.Next() {
		if indexedID(it.Key()) == id {
			out = append(out, string(it.Key()))
		}
	}
	return out
}

func has(t *testing.T, db *DB, key []byte) bool {
	t.Helper()
	ok, err := db.Has(key)
	if err != nil {
		t.Fatalf("has: %v", err)
	}
	return ok
}

func claimOne(t *testing.T, repo repository.TaskRepository) (*domain.Task, bool) {
	t.Helper()
	task, ok, err := repo.Claim(context.Background(), dlqWorker, []domain.Command{dlqCmd}, 60, 50, 3, dlqTenant)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return task, ok
}

// ---------------- requeue one task ----------------

func TestRequeueDLQTaskRestartsTheRun(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	dead := deadLetter(t, repo, dlqCmd, dlqTenant, 7)
	if dead.Status != domain.StatusFailed || dead.Attempts == 0 || dead.Error != dlqReason {
		t.Fatalf("setup: dead-lettered task %+v", dead)
	}
	if err := db.Set(KeyResult(dead.ID), []byte(`{"stale":true}`)); err != nil {
		t.Fatalf("seed result: %v", err)
	}
	waiting := enqueueTask(t, repo, 3, time.Time{}, dlqTenant)

	got, err := repo.RequeueDLQTask(ctx, dead.ID)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	stored := mustGet(t, repo, dead.ID)
	for _, task := range []*domain.Task{got, stored} {
		if task.Status != domain.StatusPending || task.Attempts != 0 || task.Error != "" ||
			task.WorkerID != "" || task.LeaseUntil != "" || task.ResultKey != "" ||
			task.LastKnownLocation != domain.LocationPending || task.Priority != 7 || task.Payload != dead.Payload {
			t.Fatalf("requeued task %+v", task)
		}
	}
	if has(t, db, KeyDLQ(dlqCmd, dlqTenant, dead.ID)) || has(t, db, KeyResult(dead.ID)) {
		t.Fatal("dead-letter entry or stale result survived the requeue")
	}
	if st := mustStats(t, repo); st.DLQ != 0 || st.Ready != 2 {
		t.Fatalf("stats after requeue %+v; want dlq 0, ready 2", st)
	}
	// Priority 7 is claimed before the priority-3 task that waited longer.
	first, ok := claimOne(t, repo)
	if !ok || first.ID != dead.ID || first.Attempts != 1 {
		t.Fatalf("first claim %+v ok=%v; want the requeued task on attempt 1", first, ok)
	}
	second, ok := claimOne(t, repo)
	if !ok || second.ID != waiting.ID {
		t.Fatalf("second claim %+v ok=%v; want %s", second, ok, waiting.ID)
	}
	if extra, ok := claimOne(t, repo); ok {
		t.Fatalf("requeued task claimable twice: %+v", extra)
	}
}

func TestRequeueDLQTaskRefusesTasksOutsideTheDLQ(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	results := NewResultRepository(db, time.UTC)
	pending := enqueueTask(t, repo, 0, time.Time{}, dlqTenant)

	failedByResult := enqueueTask(t, repo, 0, time.Time{}, dlqOtherTenant)
	if _, ok, err := repo.Claim(ctx, dlqWorker, []domain.Command{dlqCmd}, 60, 50, 3, dlqOtherTenant); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := results.UpdateTaskOnComplete(ctx, failedByResult.ID, dlqCmd, dlqOtherTenant, domain.StatusFailed, "worker said no"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	for name, id := range map[string]string{"pending": pending.ID, "failed by result": failedByResult.ID} {
		before := mustGet(t, repo, id)
		if _, err := repo.RequeueDLQTask(ctx, id); !errors.Is(err, domain.ErrTaskNotInDLQ) {
			t.Fatalf("%s: err %v, want ErrTaskNotInDLQ", name, err)
		}
		if after := mustGet(t, repo, id); after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("%s: refused requeue changed the task: %+v", name, after)
		}
	}
	if _, err := repo.RequeueDLQTask(ctx, "missing"); err == nil || err.Error() != dlqNotFound {
		t.Fatalf("missing task: err %v, want %s", err, dlqNotFound)
	}
}

// ---------------- requeue a dead-letter queue ----------------

func TestRequeueDLQDrainsOneQueueInPages(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	want := map[string]bool{}
	for i := range 5 {
		want[deadLetter(t, repo, dlqCmd, dlqTenant, i).ID] = true
	}
	other := deadLetter(t, repo, dlqCmd, dlqOtherTenant, 0)
	otherCmd := deadLetter(t, repo, domain.CmdGenerateCreative, dlqTenant, 0)

	got, calls := 0, 0
	for remaining := true; remaining; calls++ {
		if calls > 5 {
			t.Fatal("bulk requeue never drained the queue")
		}
		res, err := repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 2)
		if err != nil || res.Requeued > 2 {
			t.Fatalf("call %d: %+v, %v", calls, res, err)
		}
		got += res.Requeued
		remaining = res.Remaining
	}
	if got != len(want) || calls != 3 {
		t.Fatalf("requeued %d tasks in %d calls; want %d in 3", got, calls, len(want))
	}
	for id := range want {
		if task := mustGet(t, repo, id); task.Status != domain.StatusPending || task.Attempts != 0 {
			t.Fatalf("task %s after bulk requeue: %+v", id, task)
		}
		if keys := indexKeys(t, db, id); len(keys) != 1 || !strings.Contains(keys[0], segPending) {
			t.Fatalf("task %s index entries %q; want one pending entry", id, keys)
		}
	}
	for _, untouched := range []*domain.Task{other, otherCmd} {
		if task := mustGet(t, repo, untouched.ID); task.Status != domain.StatusFailed {
			t.Fatalf("bulk requeue reached another queue: %+v", task)
		}
	}
	res, err := repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 10)
	if err != nil || res.Requeued != 0 || res.Remaining {
		t.Fatalf("empty queue: %+v, %v", res, err)
	}
}

func TestRequeueDLQSpansCommitChunks(t *testing.T) {
	repo, _ := newDLQRepo(t)
	n := dlqRequeueChunk + 6
	for range n {
		deadLetter(t, repo, dlqCmd, dlqTenant, 0)
	}
	res, err := repo.RequeueDLQ(context.Background(), dlqCmd, dlqTenant, 1000)
	if err != nil || res.Requeued != n || res.Remaining {
		t.Fatalf("bulk requeue %+v, %v; want %d and nothing remaining", res, err, n)
	}
	if st := mustStats(t, repo); st.DLQ != 0 || st.Ready != int64(n) {
		t.Fatalf("stats %+v", st)
	}
}

// Entries whose task is gone, no longer FAILED or of another queue are
// dropped without being counted, so a drain loop always terminates.
func TestRequeueDLQDropsStaleEntries(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	live := deadLetter(t, repo, dlqCmd, dlqTenant, 0)
	reaped := deadLetter(t, repo, dlqCmd, dlqTenant, 0)
	if err := db.Delete(KeyTask(reaped.ID)); err != nil { // what the retention sweep does
		t.Fatalf("delete body: %v", err)
	}
	otherTenant := deadLetter(t, repo, dlqCmd, dlqOtherTenant, 0)
	foreign := KeyDLQ(dlqCmd, dlqTenant, otherTenant.ID) // points at another tenant's dead task
	if err := db.Set(foreign, nil); err != nil {
		t.Fatalf("seed foreign entry: %v", err)
	}

	res, err := repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 10)
	if err != nil || res.Requeued != 1 || res.Remaining {
		t.Fatalf("bulk requeue %+v, %v; want 1 and nothing remaining", res, err)
	}
	if task := mustGet(t, repo, live.ID); task.Status != domain.StatusPending {
		t.Fatalf("live task %+v", task)
	}
	if has(t, db, KeyDLQ(dlqCmd, dlqTenant, reaped.ID)) || has(t, db, foreign) {
		t.Fatal("stale dead-letter entries survived")
	}
	if task := mustGet(t, repo, otherTenant.ID); task.Status != domain.StatusFailed || !has(t, db, KeyDLQ(dlqCmd, dlqOtherTenant, otherTenant.ID)) {
		t.Fatalf("another tenant's task was touched: %+v", task)
	}
	if _, err := repo.Get(ctx, reaped.ID); err == nil {
		t.Fatal("a stale entry brought a reaped task back")
	}
}

func TestRequeueDLQSkipsHeldTasksAndLimitZeroOnlyReports(t *testing.T) {
	ctx := context.Background()
	repo, _ := newDLQRepo(t)
	held := deadLetter(t, repo, dlqCmd, dlqTenant, 0)
	free := deadLetter(t, repo, dlqCmd, dlqTenant, 0)

	res, err := repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 0)
	if err != nil || res.Requeued != 0 || !res.Remaining {
		t.Fatalf("limit 0: %+v, %v; want a report only", res, err)
	}
	if !repo.tryReserve(held.ID) {
		t.Fatal("reserve held task")
	}
	res, err = repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 10)
	if err != nil || res.Requeued != 1 || !res.Remaining {
		t.Fatalf("with a held task: %+v, %v; want 1 requeued and remaining", res, err)
	}
	if mustGet(t, repo, held.ID).Status != domain.StatusFailed || mustGet(t, repo, free.ID).Status != domain.StatusPending {
		t.Fatal("bulk requeue moved the held task or skipped the free one")
	}
	repo.release(held.ID)
	res, err = repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 10)
	if err != nil || res.Requeued != 1 || res.Remaining {
		t.Fatalf("after release: %+v, %v", res, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.RequeueDLQ(canceled, dlqCmd, dlqTenant, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: err %v", err)
	}
}

// ---------------- delete ----------------

func TestDeleteTaskRemovesTheTaskAndItsIndexEntries(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	results := NewResultRepository(db, time.UTC)

	dead := deadLetter(t, repo, dlqCmd, dlqTenant, 0)
	ready := enqueueTask(t, repo, 4, time.Time{}, dlqTenant)
	delayed := enqueueTask(t, repo, 0, time.Now().Add(time.Hour), dlqTenant)
	done := enqueueTask(t, repo, 0, time.Time{}, dlqOtherTenant)
	if _, ok, err := repo.Claim(ctx, dlqWorker, []domain.Command{dlqCmd}, 60, 50, 3, dlqOtherTenant); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := results.SaveResult(ctx, domain.ResultRecord{TaskID: done.ID, Status: domain.StatusCompleted}, dlqCmd, dlqOtherTenant); err != nil {
		t.Fatalf("save result: %v", err)
	}
	if err := results.UpdateTaskOnComplete(ctx, done.ID, dlqCmd, dlqOtherTenant, domain.StatusCompleted, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n := repo.delayedCounter(dlqCmd, dlqTenant).Load(); n != 1 {
		t.Fatalf("setup: delayed counter %d", n)
	}

	for name, task := range map[string]*domain.Task{"ready": ready, "delayed": delayed, "dead-lettered": dead, "completed": done} {
		if len(indexKeys(t, db, task.ID)) == 0 && name != "completed" {
			t.Fatalf("%s: setup has no index entry", name)
		}
		if err := repo.DeleteTask(ctx, task.ID); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
		if _, err := repo.Get(ctx, task.ID); err == nil || err.Error() != dlqNotFound {
			t.Fatalf("%s: body survived (err %v)", name, err)
		}
		if keys := indexKeys(t, db, task.ID); len(keys) != 0 || has(t, db, KeyResult(task.ID)) {
			t.Fatalf("%s: left index entries %q or a result", name, keys)
		}
		if err := repo.DeleteTask(ctx, task.ID); err == nil || err.Error() != dlqNotFound {
			t.Fatalf("%s: second delete err %v, want %s", name, err, dlqNotFound)
		}
	}
	if st := mustStats(t, repo); st != (domain.QueueStats{Command: dlqCmd}) {
		t.Fatalf("stats after deleting everything %+v", st)
	}
	if n := repo.delayedCounter(dlqCmd, dlqTenant).Load(); n != 0 {
		t.Fatalf("delayed counter %d after deleting the delayed task", n)
	}
	if _, ok := claimOne(t, repo); ok {
		t.Fatal("a deleted task was claimed through its stale hint")
	}
}

func TestDeleteTaskRefusesAnInProgressTask(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	task := enqueueTask(t, repo, 0, time.Time{}, dlqTenant)
	if _, ok := claimOne(t, repo); !ok {
		t.Fatal("claim")
	}
	if err := repo.DeleteTask(ctx, task.ID); !errors.Is(err, domain.ErrTaskInProgress) {
		t.Fatalf("err %v, want ErrTaskInProgress", err)
	}
	if got := mustGet(t, repo, task.ID); got.Status != domain.StatusInProgress || len(indexKeys(t, db, task.ID)) != 1 {
		t.Fatalf("refused delete changed the task: %+v", got)
	}
}

// A claim in flight holds the task: the delete waits for it and then sees
// the task in progress. A context that ends first stops the wait.
func TestDeleteTaskWaitsForAClaimInFlight(t *testing.T) {
	repo, _ := newDLQRepo(t)
	task := enqueueTask(t, repo, 0, time.Time{}, dlqTenant)
	if !repo.tryReserve(task.ID) {
		t.Fatal("reserve")
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := repo.DeleteTask(short, task.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the context deadline", err)
	}
	repo.release(task.ID)
	if err := repo.DeleteTask(context.Background(), task.ID); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

// The delayed sweep holds its bucket's flag; a delete of a delayed task in
// that bucket waits for it.
func TestDeleteDelayedTaskWaitsForTheSweep(t *testing.T) {
	repo, _ := newDLQRepo(t)
	task := enqueueTask(t, repo, 0, time.Now().Add(time.Hour), dlqTenant)
	flag := repo.delayedMoveFlagFor(dlqCmd, dlqTenant)
	flag.Store(1)
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := repo.DeleteTask(short, task.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the context deadline", err)
	}
	mustGet(t, repo, task.ID)
	flag.Store(0)
	if err := repo.DeleteTask(context.Background(), task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if flag.Load() != 0 {
		t.Fatal("delete kept the delayed-move flag")
	}
}

// switchReplicator applies writes locally like a leader until fail is set.
type switchReplicator struct {
	db   *pebbledb.DB
	fail atomic.Bool
}

func (s *switchReplicator) IsLeader() bool         { return true }
func (s *switchReplicator) LeaderHTTPAddr() string { return "" }
func (s *switchReplicator) Replicate(repr []byte) error {
	if s.fail.Load() {
		return errors.New("replication failed")
	}
	b := s.db.NewBatch()
	defer b.Close()
	if err := b.SetRepr(bytes.Clone(repr)); err != nil {
		return err
	}
	return s.db.Apply(b, pebbledb.NoSync)
}

// A claim that pops the hint of a reserved task drops it; a delete that
// then fails must put the hint back, or the task is never claimed again.
func TestFailedDeleteRepublishesTheClaimHint(t *testing.T) {
	db := openTestDB(t)
	repl := &switchReplicator{db: db.Raw()}
	db.AttachReplicator(repl)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	task := enqueueTask(t, repo, 0, time.Time{}, dlqTenant)

	if !repo.tryReserve(task.ID) {
		t.Fatal("reserve")
	}
	if _, ok := claimOne(t, repo); ok {
		t.Fatal("claimed a reserved task")
	}
	repo.release(task.ID)

	repl.fail.Store(true)
	if err := repo.DeleteTask(context.Background(), task.ID); err == nil {
		t.Fatal("delete succeeded with replication failing")
	}
	repl.fail.Store(false)
	if got, ok := claimOne(t, repo); !ok || got.ID != task.ID {
		t.Fatalf("claim after failed delete = %+v, %v; want the task", got, ok)
	}
}

// Deletes racing claims on the same ready tasks: every task ends either
// claimed (and kept) or deleted (and never claimed), with no stray entry.
func TestDeleteTaskRacingClaims(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	const n = 200
	ids := make([]string, n)
	for i := range ids {
		ids[i] = enqueueTask(t, repo, i%3, time.Time{}, dlqTenant).ID
	}
	var claimed sync.Map
	var deleted sync.Map
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task, ok, err := repo.Claim(ctx, dlqWorker, []domain.Command{dlqCmd}, 60, 50, 3, dlqTenant)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if !ok {
					return
				}
				claimed.Store(task.ID, true)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, id := range ids {
			switch err := repo.DeleteTask(ctx, id); {
			case err == nil:
				deleted.Store(id, true)
			case !errors.Is(err, domain.ErrTaskInProgress):
				t.Errorf("delete %s: %v", id, err)
			}
		}
	}()
	wg.Wait()

	for _, id := range ids {
		_, wasClaimed := claimed.Load(id)
		_, wasDeleted := deleted.Load(id)
		task, err := repo.Get(ctx, id)
		switch {
		case wasClaimed == wasDeleted:
			t.Fatalf("task %s claimed=%v deleted=%v; want exactly one", id, wasClaimed, wasDeleted)
		case wasDeleted && (err == nil || len(indexKeys(t, db, id)) != 0):
			t.Fatalf("deleted task %s came back: %+v", id, task)
		case wasClaimed && (err != nil || task.Status != domain.StatusInProgress):
			t.Fatalf("claimed task %s: %+v, %v", id, task, err)
		}
	}
}

// Bulk requeues racing deletes of the same dead-lettered tasks: every task
// ends either requeued with one pending entry or deleted with none.
func TestRequeueDLQRacingDeletes(t *testing.T) {
	ctx := context.Background()
	repo, db := newDLQRepo(t)
	const n = 100
	ids := make([]string, n)
	for i := range ids {
		ids[i] = deadLetter(t, repo, dlqCmd, dlqTenant, 0).ID
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for remaining := true; remaining; {
			res, err := repo.RequeueDLQ(ctx, dlqCmd, dlqTenant, 7)
			if err != nil {
				t.Errorf("bulk requeue: %v", err)
				return
			}
			remaining = res.Remaining
		}
	}()
	go func() {
		defer wg.Done()
		for _, id := range ids {
			if err := repo.DeleteTask(ctx, id); err != nil {
				t.Errorf("delete %s: %v", id, err)
			}
		}
	}()
	wg.Wait()
	for _, id := range ids {
		if keys := indexKeys(t, db, id); len(keys) != 0 {
			t.Fatalf("deleted task %s left %q", id, keys)
		}
		if _, err := repo.Get(ctx, id); err == nil {
			t.Fatalf("task %s survived its delete", id)
		}
	}
}

// ---------------- sharded ----------------

func newShardedDLQRepo(t *testing.T, n int) (*ShardedTaskRepository, []*TaskRepository) {
	t.Helper()
	shards := make([]*TaskRepository, n)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	return NewShardedTaskRepository(shards), shards
}

func TestShardedDLQOperationsRouteToTheOwningShard(t *testing.T) {
	ctx := context.Background()
	sharded, shards := newShardedDLQRepo(t, 4)
	dead := make([]*domain.Task, 0, 8)
	for range 8 {
		dead = append(dead, deadLetter(t, sharded, dlqCmd, dlqTenant, 0))
	}
	for i, task := range dead {
		owner := shards[sharded.shardOf(task.ID)]
		if i%2 == 0 {
			if _, err := sharded.RequeueDLQTask(ctx, task.ID); err != nil {
				t.Fatalf("requeue %s: %v", task.ID, err)
			}
			if got := mustGet(t, owner, task.ID); got.Status != domain.StatusPending {
				t.Fatalf("owner shard holds %+v", got)
			}
			continue
		}
		if err := sharded.DeleteTask(ctx, task.ID); err != nil {
			t.Fatalf("delete %s: %v", task.ID, err)
		}
		if _, err := owner.Get(ctx, task.ID); err == nil {
			t.Fatalf("task %s still on its shard", task.ID)
		}
	}
	if st := mustStats(t, sharded); st.DLQ != 0 || st.Ready != 4 {
		t.Fatalf("stats %+v; want dlq 0, ready 4", st)
	}
}

func TestShardedRequeueDLQWalksEveryShard(t *testing.T) {
	ctx := context.Background()
	sharded, _ := newShardedDLQRepo(t, 3)
	const n = 9
	for range n {
		deadLetter(t, sharded, dlqCmd, dlqTenant, 0)
	}
	got := 0
	for calls := 0; ; calls++ {
		if calls > n {
			t.Fatal("bulk requeue never drained the shards")
		}
		res, err := sharded.RequeueDLQ(ctx, dlqCmd, dlqTenant, 2)
		if err != nil || res.Requeued > 2 {
			t.Fatalf("call %d: %+v, %v", calls, res, err)
		}
		got += res.Requeued
		if !res.Remaining {
			break
		}
	}
	if got != n {
		t.Fatalf("requeued %d of %d", got, n)
	}
}

// followerReplicator makes a shard read-only, like a Raft follower.
type followerReplicator struct{}

func (followerReplicator) IsLeader() bool         { return false }
func (followerReplicator) LeaderHTTPAddr() string { return "http://leader-of-shard" }
func (followerReplicator) Replicate([]byte) error { return ErrNotLeader }

// A shard led elsewhere keeps Remaining true; once the led shards are empty
// the call returns that shard's not-leader error so it can be forwarded.
func TestShardedRequeueDLQDefersShardsLedElsewhere(t *testing.T) {
	ctx := context.Background()
	sharded, shards := newShardedDLQRepo(t, 2)
	perShard := map[int]int{}
	for perShard[0] == 0 || perShard[1] == 0 {
		task := deadLetter(t, sharded, dlqCmd, dlqTenant, 0)
		perShard[sharded.shardOf(task.ID)]++
	}
	shards[1].db.AttachReplicator(followerReplicator{})

	res, err := sharded.RequeueDLQ(ctx, dlqCmd, dlqTenant, 1000)
	if err != nil || res.Requeued != perShard[0] || !res.Remaining {
		t.Fatalf("first call %+v, %v; want %d requeued and remaining", res, err, perShard[0])
	}
	_, err = sharded.RequeueDLQ(ctx, dlqCmd, dlqTenant, 1000)
	var notLeader *NotLeaderError
	if !errors.As(err, &notLeader) || notLeader.LeaderURL != "http://leader-of-shard" {
		t.Fatalf("second call err %v; want the follower shard's not-leader error", err)
	}
	if st := mustStats(t, shards[1]); st.DLQ != int64(perShard[1]) {
		t.Fatalf("follower shard was written: %+v", st)
	}
}
