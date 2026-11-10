package pebble

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// A leader stuck before its dispatch rebuild leaves claims failing and no
// lease or TTL swept. Deferred ticks are routed through the stall watch: after
// a minute of continuous deferral the reaper logs one rate-limited WARN that
// names the sweep and the error class, never the error text.
func TestReaperWarnsWhenDeferredForAMinute(t *testing.T) {
	db := openTestDB(t)
	repl := &barrierReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, observe: make(chan bool)}
	t.Cleanup(func() { close(repl.observe) })
	repl.epoch.Store(1)
	repl.failBarrier.Store(true)
	db.AttachReplicator(repl)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)

	var logs bytes.Buffer
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	r := NewReaper(db, time.UTC, slog.New(slog.NewTextHandler(&logs, nil)), ReaperOptions{LeaderReady: repo.EnsureLeaderDispatch})
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	ctx := context.Background()
	var stall stallWatch

	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	if strings.Contains(logs.String(), "codeq_reaper_deferred") {
		t.Fatal("warned on the first deferred tick")
	}
	clock.Add(int64(stallWarnAfter))
	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	out := logs.String()
	if !strings.Contains(out, "event=codeq_reaper_deferred") || !strings.Contains(out, "sweep=lease") ||
		!strings.Contains(out, "errorClass="+errClassOther) || !strings.Contains(out, "deferredFor=1m0s") {
		t.Fatalf("sustained deferral not reported: %s", out)
	}
	if strings.Contains(out, "barrier timed out") {
		t.Fatalf("deferral log carries the error text: %s", out)
	}
	logs.Reset()
	clock.Add(int64(time.Minute))
	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	if strings.Contains(logs.String(), "codeq_reaper_deferred") {
		t.Fatal("deferral warning not rate limited")
	}

	// Once the barrier completes the tick runs and the deferral ends.
	repl.failBarrier.Store(false)
	r.tickWhenReady(ctx, r.leaseSweep, sweepLease, &stall)
	if !stall.deferred.since.IsZero() {
		t.Fatal("a tick that ran did not end the deferral")
	}
}

func TestWarnRebuildFailureSkipsLostLeadershipAndRateLimits(t *testing.T) {
	var last time.Time
	t0 := time.Unix(1_000_000, 0)
	if warnRebuildFailure(nil, t0, &last) {
		t.Fatal("a successful rebuild warned")
	}
	if warnRebuildFailure(&NotLeaderError{LeaderURL: "http://peer"}, t0, &last) {
		t.Fatal("a lost leadership warned")
	}
	failure := errors.New("barrier timed out")
	if !warnRebuildFailure(failure, t0, &last) {
		t.Fatal("a failed rebuild did not warn")
	}
	if warnRebuildFailure(failure, t0.Add(rebuildWarnEvery-time.Second), &last) {
		t.Fatal("rebuild warning not rate limited")
	}
	if !warnRebuildFailure(failure, t0.Add(rebuildWarnEvery), &last) {
		t.Fatal("rebuild warning not repeated after the interval")
	}
}

func TestDispatchErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, errClassNone},
		{&NotLeaderError{}, errClassNotLeader},
		{ErrNotLeader, errClassNotLeader},
		{context.DeadlineExceeded, errClassTimeout},
		{context.Canceled, errClassCanceled},
		{errors.New("tenant-a secret detail"), errClassOther},
	} {
		if got := dispatchErrorClass(tc.err); got != tc.want {
			t.Errorf("class(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}
