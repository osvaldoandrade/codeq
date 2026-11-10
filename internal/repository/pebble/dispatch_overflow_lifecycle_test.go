package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// requireOnce fails when a task was claimed more than once.
func requireOnce(t *testing.T, got []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(got))
	for _, id := range got {
		if _, dup := seen[id]; dup {
			t.Fatalf("task %s claimed twice in %v", id, got)
		}
		seen[id] = struct{}{}
	}
}

// A leader's seq counter is seeded at Open; after a failover the rebuild is
// what raises it past the committed backlog. The keys the cap refuses are the
// newest, highest seqs, so the rebuild must raise the counter for them too:
// otherwise the new leader numbers its next task inside the backlog and it is
// claimed before older tasks (FIFO inversion).
func TestRebuildRaisesSeqPastOverflowedBacklog(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("failover-topic")
	want := enqueueN(t, repo, cmd, 10)

	// A follower that wins the election never enqueued: its counter is at
	// the value it opened with.
	db.seq.Store(0)
	repo.dispatchMu.Lock()
	err := repo.rebuildLocked()
	repo.dispatchMu.Unlock()
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	want = append(want, enqueueN(t, repo, cmd, 1)...)
	got := claimAll(t, repo, cmd)
	requireOnce(t, got)
	requireOrder(t, got, want)
	requireEmptyDispatch(t, repo)
}

// A lease that expires on a task of an overflowed queue goes through the
// delayed set (backoff) and returns with a new seq: it is claimed once, after
// every task that was waiting, and nothing is left behind.
func TestLeaseExpiryRequeueIntoOverflowedQueue(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("lease-topic")
	ids := enqueueN(t, repo, cmd, 6)
	first, ok, err := repo.Claim(context.Background(), "lost-worker", []domain.Command{cmd}, 60, 200, 5, "tenant-a")
	if err != nil || !ok || first.ID != ids[0] {
		t.Fatalf("first claim: %+v ok=%v err=%v", first, ok, err)
	}
	if f := repo.DispatchFootprint(); f.Overflowed != 1 {
		t.Fatalf("queue not overflowed after 6 publishes at cap %d: %+v", overflowCap, f)
	}

	base := time.Now()
	repo.clock = func() time.Time { return base.Add(2 * time.Minute) }
	if n, err := repo.requeueExpired(context.Background(), cmd, 10, 5, "tenant-a"); err != nil || n != 1 {
		t.Fatalf("requeue expired: n=%d err=%v", n, err)
	}
	// Past the 1 s fixed backoff: the claims move it back to pending.
	repo.clock = func() time.Time { return base.Add(3 * time.Minute) }
	got := claimAll(t, repo, cmd)
	requireOnce(t, got)
	requireOrder(t, got, append(append([]string{}, ids[1:]...), ids[0]))
	requireEmptyDispatch(t, repo)
	if task, err := repo.Get(context.Background(), ids[0]); err != nil || task.WorkerID != "worker" || task.Status != domain.StatusInProgress {
		t.Fatalf("redelivered task: %+v err=%v", task, err)
	}
}

// A delayed task that becomes due while its queue is overflowed is refused a
// hint like any publish; the refill finds its new pending key in order, after
// the tasks already waiting and before the ones published later.
func TestDelayedMoveIntoOverflowedQueue(t *testing.T) {
	db := openTestDB(t)
	repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	cmd := domain.Command("delayed-topic")
	ctx := context.Background()
	base := time.Now()
	repo.clock = func() time.Time { return base }
	delayed, err := repo.Enqueue(ctx, cmd, `{"delayed":true}`, 4, "", 5, "", "", "", base.Add(30*time.Second), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	ready := enqueueN(t, repo, cmd, 5)
	head, ok, err := repo.Claim(ctx, "worker", []domain.Command{cmd}, 60, 200, 5, "tenant-a")
	if err != nil || !ok || head.ID != ready[0] {
		t.Fatalf("claim before the delay: %+v ok=%v err=%v", head, ok, err)
	}

	repo.clock = func() time.Time { return base.Add(time.Minute) }
	if n, err := repo.moveDueDelayedForTenant(ctx, cmd, 10, "tenant-a"); err != nil || n != 1 {
		t.Fatalf("move due delayed: n=%d err=%v", n, err)
	}
	if f := repo.DispatchFootprint(); f.Overflowed != 1 || f.Hints != 2 {
		t.Fatalf("the moved task got a hint on an overflowed queue: %+v", f)
	}
	later := enqueueN(t, repo, cmd, 1)
	want := append(append(append([]string{}, ready[1:]...), delayed.ID), later...)
	got := claimAll(t, repo, cmd)
	requireOnce(t, got)
	requireOrder(t, got, want)
	requireEmptyDispatch(t, repo)
}

// Absolute-expiry rows retire while their queue is overflowed, by the sweep
// or by the claim that meets them: refills skip the retired keys, and every
// live task is claimed once, in order.
func TestAbsoluteExpiryRetirementWhileOverflowed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sweep bool
	}{
		{name: "sweep", sweep: true},
		{name: "claim", sweep: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, _, clock := absoluteFixture(t)
			repo := newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
			base := time.Unix(0, clock.Load()).UTC()
			var expired, live []string
			for i := 0; i < 4; i++ {
				e, l := "expiring-"+string(rune('a'+i)), "live-"+string(rune('a'+i))
				enqueueAbsoluteAt(t, repo, e, base.Add(time.Minute))
				enqueueAbsoluteAt(t, repo, l, base.Add(10*time.Minute))
				expired, live = append(expired, e), append(live, l)
			}
			if f := repo.DispatchFootprint(); f.Overflowed != 1 || f.Hints != overflowCap {
				t.Fatalf("8 publishes at cap %d: %+v", overflowCap, f)
			}
			clock.Store(base.Add(61 * time.Second).UnixNano())
			if tc.sweep {
				if n, err := db.sweepAbsolute(context.Background(), 100); err != nil || n != len(expired) {
					t.Fatalf("sweep: removed=%d err=%v", n, err)
				}
			}
			got := claimAll(t, repo, absoluteTestTopic)
			requireOnce(t, got)
			requireOrder(t, got, live)
			requireEmptyDispatch(t, repo)
			for _, id := range expired {
				assertNoPayloadIndexes(t, db, id)
			}
		})
	}
}
