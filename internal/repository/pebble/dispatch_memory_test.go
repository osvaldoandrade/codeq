package pebble

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// hintsHeld reports the hints queued for one (cmd, prio) of tenant-a, the
// tenant of every test that inspects a queue.
func (r *TaskRepository) hintsHeld(cmd domain.Command, prio int) int {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if q := r.queues[queueKey(cmd, "tenant-a", prio)]; q != nil {
		return q.len()
	}
	return 0
}

func retainedHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// Regression for the leader runaway (ADR 0015). Claims run only on the Raft
// leader and probe every priority of every command they name. Each probe used
// to create a 100,000-slot hint channel (2.4 MB of pointer-bearing heap) that
// was never freed, so a leader serving one claim per topic for Conveste's 13
// topics retained ~300 MiB: past its 358 MiB Go soft limit once Pebble's block
// cache was added, which kept the GC running back to back at its CPU cap,
// starved Raft and request handling, and ended in an OOM kill. A probe of an
// empty priority must cost no memory.
func TestClaimProbesRetainNoDispatchMemory(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	ctx := context.Background()
	const topics = 13
	before := retainedHeap()
	for i := 0; i < topics; i++ {
		cmd := domain.Command(fmt.Sprintf("topic-%02d", i))
		if _, ok, err := repo.Claim(ctx, "worker", []domain.Command{cmd}, 60, 200, 5, "conveste"); err != nil || ok {
			t.Fatalf("claim %s on an empty topic: ok=%v err=%v", cmd, ok, err)
		}
		if got, err := repo.ClaimMany(ctx, "worker", []domain.Command{cmd}, 60, 10, 200, 5, "conveste"); err != nil || len(got) != 0 {
			t.Fatalf("claim-many %s on an empty topic: %d tasks err=%v", cmd, len(got), err)
		}
	}
	after := retainedHeap()
	// The repository (the leader's dispatch index) must stay reachable
	// while its heap is measured.
	runtime.KeepAlive(repo)
	if grown := int64(after) - int64(before); grown > 16<<20 {
		t.Fatalf("claims on %d empty topics retained %d MiB of heap; probes must not allocate hint storage", topics, grown>>20)
	}
	if f := repo.DispatchFootprint(); f.Queues != 0 || f.Hints != 0 || f.InFlight != 0 {
		t.Fatalf("empty probes left dispatch state behind: %+v", f)
	}
}

// A queue holds memory only while it holds hints: claiming every task drops
// it, and the next publish creates it again.
func TestHintQueueReleasedWhenDrained(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	ctx := context.Background()
	cmd := domain.Command("drain-topic")
	for i := 0; i < 3; i++ {
		if _, err := repo.Enqueue(ctx, cmd, fmt.Sprintf(`{"n":%d}`, i), 3, "", 5, "", "", "", time.Time{}, "tenant-a"); err != nil {
			t.Fatal(err)
		}
	}
	if f := repo.DispatchFootprint(); f.Queues != 1 || f.Hints != 3 {
		t.Fatalf("after three enqueues footprint=%+v, want one queue with three hints", f)
	}
	for i := 0; i < 3; i++ {
		if _, ok, err := repo.Claim(ctx, "worker", []domain.Command{cmd}, 60, 200, 5, "tenant-a"); err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
	}
	if f := repo.DispatchFootprint(); f.Queues != 0 || f.Hints != 0 || f.InFlight != 0 {
		t.Fatalf("drained queue still held: %+v", f)
	}
	if _, err := repo.Enqueue(ctx, cmd, `{"n":3}`, 3, "", 5, "", "", "", time.Time{}, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if got := repo.hintsHeld(cmd, 3); got != 1 {
		t.Fatalf("republished queue holds %d hints, want 1", got)
	}
}

// The queue is FIFO across compaction of its consumed prefix and never keeps
// a popped id reachable.
func TestHintQueueFIFOAcrossCompaction(t *testing.T) {
	q := &hintQueue{}
	next, want := 0, 0
	push := func(n int) {
		for i := 0; i < n; i++ {
			q.push(pendingHint{seq: uint64(next), id: fmt.Sprint(next)})
			next++
		}
	}
	pop := func(n int) {
		for i := 0; i < n; i++ {
			h, ok := q.pop()
			if !ok || h.seq != uint64(want) || h.id != fmt.Sprint(want) {
				t.Fatalf("pop %d = %+v ok=%v", want, h, ok)
			}
			want++
		}
	}
	push(3 * hintQueueCompactAt)
	pop(2 * hintQueueCompactAt)
	if q.head >= hintQueueCompactAt {
		t.Fatalf("consumed prefix of %d not compacted", q.head)
	}
	for i := 0; i < q.head; i++ {
		if q.items[i].id != "" {
			t.Fatalf("popped slot %d still references %q", i, q.items[i].id)
		}
	}
	push(hintQueueCompactAt)
	pop(q.len())
	if q.items != nil || q.head != 0 {
		t.Fatalf("drained queue kept its array: len=%d head=%d", len(q.items), q.head)
	}
	if _, ok := q.pop(); ok {
		t.Fatal("pop from an empty queue succeeded")
	}
}

// The per-queue cap of the former channel is kept: a full queue refuses a
// hint (its pending key stays durable for the next rebuild).
func TestHintQueueRefusesBeyondCap(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	repo.dispatchMu.Lock()
	defer repo.dispatchMu.Unlock()
	for i := 0; i < maxHintsPerQueue; i++ {
		if !repo.sendHintLocked("cap-topic", "tenant-a", 0, uint64(i+1), fmt.Sprintf("id-%d", i)) {
			t.Fatalf("hint %d refused below the cap", i)
		}
	}
	if repo.sendHintLocked("cap-topic", "tenant-a", 0, maxHintsPerQueue+1, "one-too-many") {
		t.Fatal("full queue accepted a hint")
	}
	if !repo.sendHintLocked("cap-topic", "tenant-a", 1, 1, "other-priority") {
		t.Fatal("a full queue blocked another priority")
	}
}

// Concurrent publishers and claimers deliver every task exactly once and
// leave no dispatch memory behind.
func TestDispatchConcurrentPublishAndClaimLeavesNothing(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	ctx := context.Background()
	const producers, perProducer, claimers = 4, 50, 4
	cmds := []domain.Command{"race-a", "race-b"}
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				if _, err := repo.Enqueue(ctx, cmds[i%2], fmt.Sprintf(`{"p":%d,"i":%d}`, p, i), i%10, "", 5, "", "", "", time.Time{}, "tenant-a"); err != nil {
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
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if time.Now().After(deadline) {
					return
				}
				task, ok, err := repo.Claim(ctx, "worker", cmds, 60, 200, 5, "tenant-a")
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					continue
				}
				if _, dup := claimed.LoadOrStore(task.ID, true); dup {
					t.Errorf("task %s claimed twice", task.ID)
				}
				if count.Add(1) == producers*perProducer {
					close(done)
				}
			}
		}()
	}
	wg.Wait()
	if got := count.Load(); got != producers*perProducer {
		t.Fatalf("claimed %d of %d tasks", got, producers*perProducer)
	}
	if f := repo.DispatchFootprint(); f.Queues != 0 || f.Hints != 0 || f.InFlight != 0 {
		t.Fatalf("dispatch memory left after every task was claimed: %+v", f)
	}
}
