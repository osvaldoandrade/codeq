package pebble

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// overflowCap is the hint cap the overflow tests run with, so a queue deeper
// than the cap needs a handful of tasks instead of 100,000.
const overflowCap = 3

func enqueueN(t *testing.T, repo *TaskRepository, cmd domain.Command, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		task, err := repo.Enqueue(context.Background(), cmd, fmt.Sprintf(`{"i":%d}`, i), 4, "", 5, "", "", "", time.Time{}, "tenant-a")
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		ids = append(ids, task.ID)
	}
	return ids
}

// claimAll claims one task at a time until none is left and returns the
// claimed IDs in claim order.
func claimAll(t *testing.T, repo *TaskRepository, cmd domain.Command) []string {
	t.Helper()
	var got []string
	for i := 0; i < 1000; i++ {
		task, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 200, 5, "tenant-a")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if !ok {
			return got
		}
		got = append(got, task.ID)
	}
	t.Fatal("claims never ran out")
	return nil
}

func requireOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("claimed %d tasks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claim %d took %s, want %s (FIFO order broken)", i, got[i], want[i])
		}
	}
}

func requireEmptyDispatch(t *testing.T, repo *TaskRepository) {
	t.Helper()
	if f := repo.DispatchFootprint(); f.Queues != 0 || f.Hints != 0 || f.InFlight != 0 || f.Overflowed != 0 {
		t.Fatalf("dispatch state left after every task was claimed: %+v", f)
	}
}

// A restart that finds more pending tasks in one (command, tenant, priority)
// than the hint cap used to panic in the constructor (rebuild error), so
// every voter crash-looped. It now seeds the oldest tasks up to the cap,
// marks the queue overflowed, and claims refill it so every task is claimed
// in FIFO order.
func TestRestartWithMorePendingThanCapSeedsCapAndDrainsInOrder(t *testing.T) {
	db := openTestDB(t)
	cmd := domain.Command("deep-topic")
	want := enqueueN(t, NewTaskRepository(db, time.UTC, "fixed", 1, 5), cmd, 10)

	var repo *TaskRepository
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("constructor panicked on a queue deeper than the cap: %v", p)
			}
		}()
		repo = newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	}()
	if f := repo.DispatchFootprint(); f.Hints != overflowCap || f.Overflowed != 1 || f.Queues != 1 {
		t.Fatalf("restart seeded %+v, want %d hints in 1 overflowed queue", f, overflowCap)
	}
	requireOrder(t, claimAll(t, repo, cmd), want)
	requireEmptyDispatch(t, repo)
}

// A leadership rebuild (rebuildLocked) over a queue deeper than the cap
// succeeds instead of failing every claim and, since ADR 0015, every reaper
// sweep.
func TestRebuildWithMorePendingThanCapSucceeds(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("deep-topic")
	want := enqueueN(t, repo, cmd, 8)

	repo.dispatchMu.Lock()
	err := repo.rebuildLocked()
	repo.dispatchMu.Unlock()
	if err != nil {
		t.Fatalf("rebuild over a deep queue failed: %v", err)
	}
	if f := repo.DispatchFootprint(); f.Hints != overflowCap || f.Overflowed != 1 {
		t.Fatalf("rebuild seeded %+v, want %d hints and one overflowed queue", f, overflowCap)
	}
	requireOrder(t, claimAll(t, repo, cmd), want)
	requireEmptyDispatch(t, repo)
}

// Publishes refused at the cap are recovered the same way: an overflowed
// queue accepts no new hint, so tasks published while it drains wait behind
// the refused ones and FIFO order is the order of the pending keys.
func TestPublishBeyondCapRefillsInOrder(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("busy-topic")
	want := enqueueN(t, repo, cmd, 5)
	if f := repo.DispatchFootprint(); f.Hints != overflowCap || f.Overflowed != 1 {
		t.Fatalf("after 5 publishes: %+v", f)
	}
	var got []string
	for i := 0; i < 2; i++ {
		task, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 200, 5, "tenant-a")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		got = append(got, task.ID)
	}
	// The queue holds one hint again, but it is overflowed: these wait.
	want = append(want, enqueueN(t, repo, cmd, 3)...)
	if f := repo.DispatchFootprint(); f.Hints != 1 {
		t.Fatalf("an overflowed queue accepted new hints: %+v", f)
	}
	got = append(got, claimAll(t, repo, cmd)...)
	requireOrder(t, got, want)
	requireEmptyDispatch(t, repo)
}

// ClaimMany pops through refills within one call: it skips the hints it
// already holds in flight and returns every task in order.
func TestClaimManyDrainsOverflowedQueueInOrder(t *testing.T) {
	db := openTestDB(t)
	cmd := domain.Command("batch-topic")
	want := enqueueN(t, NewTaskRepository(db, time.UTC, "fixed", 1, 5), cmd, 10)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	tasks, err := repo.ClaimMany(context.Background(), "worker", []domain.Command{cmd}, 60, 20, 200, 5, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(tasks))
	for _, task := range tasks {
		got = append(got, task.ID)
	}
	requireOrder(t, got, want)
	requireEmptyDispatch(t, repo)
}

// A claim whose commit fails republishes its hint; on an overflowed queue the
// republish is refused and the refill finds the pending key again, so the
// task is not lost.
func TestRepublishOnOverflowedQueueIsRefilled(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("retry-topic")
	want := enqueueN(t, repo, cmd, 5)
	h, ok := repo.popHint(cmd, "tenant-a", 4)
	if !ok || h.id != want[0] {
		t.Fatalf("pop = %+v %v, want %s", h, ok, want[0])
	}
	// As a failed claim does: finish the hint and republish it. The refused
	// republish is found again by the refill, in pending-key order.
	repo.finishHint(cmd, "tenant-a", 4, h, true)
	requireOrder(t, claimAll(t, repo, cmd), []string{want[1], want[2], want[0], want[3], want[4]})
	requireEmptyDispatch(t, repo)
}

// Concurrent producers and claimers over queues far smaller than their load
// still deliver every task exactly once and leave nothing behind.
func TestOverflowConcurrentPublishAndClaimDeliversEveryTaskOnce(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	ctx := context.Background()
	const producers, perProducer, claimers = 4, 40, 4
	cmds := []domain.Command{"of-a", "of-b"}
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				if _, err := repo.Enqueue(ctx, cmds[i%2], fmt.Sprintf(`{"p":%d,"i":%d}`, p, i), i%3, "", 5, "", "", "", time.Time{}, "tenant-a"); err != nil {
					t.Error(err)
					return
				}
			}
		}(p)
	}
	var claimed sync.Map
	var count atomic.Int64
	done := make(chan struct{})
	deadline := time.Now().Add(30 * time.Second)
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				select {
				case <-done:
					return
				default:
				}
				var got []*domain.Task
				if c%2 == 0 {
					task, ok, err := repo.Claim(ctx, "worker", cmds, 60, 200, 5, "tenant-a")
					if err != nil {
						t.Error(err)
						return
					}
					if ok {
						got = append(got, task)
					}
				} else {
					many, err := repo.ClaimMany(ctx, "worker", cmds, 60, 4, 200, 5, "tenant-a")
					if err != nil {
						t.Error(err)
						return
					}
					got = many
				}
				for _, task := range got {
					if _, dup := claimed.LoadOrStore(task.ID, true); dup {
						t.Errorf("task %s claimed twice", task.ID)
					}
					if count.Add(1) == producers*perProducer {
						close(done)
					}
				}
			}
		}(c)
	}
	wg.Wait()
	if got := count.Load(); got != producers*perProducer {
		t.Fatalf("claimed %d of %d tasks", got, producers*perProducer)
	}
	requireEmptyDispatch(t, repo)
}

// The storage monitor samples the footprint every 5 s; it must not wait
// while a rebuild or a refill holds dispatchMu.
func TestDispatchFootprintDoesNotWaitForDispatchLock(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	enqueueN(t, repo, "sampled-topic", 2)
	repo.dispatchMu.Lock()
	defer repo.dispatchMu.Unlock()
	got := make(chan DispatchFootprint, 1)
	go func() { got <- repo.DispatchFootprint() }()
	select {
	case f := <-got:
		if f.Hints != 2 || f.Queues != 1 {
			t.Fatalf("footprint = %+v, want 2 hints in 1 queue", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DispatchFootprint waited on dispatchMu")
	}
}
