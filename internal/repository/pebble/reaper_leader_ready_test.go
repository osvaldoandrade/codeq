package pebble

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// barrierReplicator is a leader whose Raft barrier can be made to fail, as a
// voter that has just restored a snapshot and not yet applied the committed
// log behind it would time out its barrier.
type barrierReplicator struct {
	*ambiguousAbsoluteReplicator
	epoch       atomic.Uint64
	failBarrier atomic.Bool
	barriers    atomic.Int64
	observe     chan bool
}

func (r *barrierReplicator) LeaderObservation() <-chan bool { return r.observe }
func (r *barrierReplicator) LeadershipEpoch() uint64        { return r.epoch.Load() }
func (r *barrierReplicator) Barrier(context.Context) error {
	r.barriers.Add(1)
	if r.failBarrier.Load() {
		return errors.New("barrier timed out: log not applied yet")
	}
	return nil
}

// A new leadership term must not sweep before its barrier has applied the
// committed log and the lease table has been rebuilt from it: the tick is
// deferred, writes nothing, and runs once the barrier succeeds.
func TestReaperDefersUntilLeaderAppliedItsLog(t *testing.T) {
	db := openTestDB(t)
	repl := &barrierReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, observe: make(chan bool)}
	t.Cleanup(func() { close(repl.observe) })
	repl.epoch.Store(1)
	db.AttachReplicator(repl)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	ctx := context.Background()
	cmd := domain.CmdGenerateMaster
	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 0, "", 5, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The durable lease is already over, so the rebuilt lease table holds
	// it as expired.
	repo.clock = func() time.Time { return time.Now().Add(-time.Hour) }
	if _, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 30, 10, 5, ""); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	repo.clock = nil

	// A new term on this process whose barrier cannot complete yet.
	repl.epoch.Add(1)
	repl.failBarrier.Store(true)
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{MaxAttemptsDefault: 5, LeaderReady: repo.EnsureLeaderDispatch})
	deferredBefore := reaperTicks(t, sweepLease, outcomeDeferred)
	var stall stallWatch
	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	if got := reaperTicks(t, sweepLease, outcomeDeferred) - deferredBefore; got != 1 {
		t.Fatalf("deferred ticks=%v, want 1", got)
	}
	if got, err := repo.Get(ctx, task.ID); err != nil || got.Status != domain.StatusInProgress {
		t.Fatalf("a tick before the barrier changed the task: %+v err=%v", got, err)
	}

	repl.failBarrier.Store(false)
	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	got, err := repo.Get(ctx, task.ID)
	if err != nil || got.Status != domain.StatusPending || got.LastKnownLocation != domain.LocationDelayed {
		t.Fatalf("expired lease not requeued after the barrier: %+v err=%v", got, err)
	}
}
