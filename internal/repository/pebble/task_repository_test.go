package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnqueueClaimComplete(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	enq, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if enq.Status != domain.StatusPending {
		t.Fatalf("expected status PENDING, got %s", enq.Status)
	}

	got, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if got.ID != enq.ID {
		t.Fatalf("expected %s, got %s", enq.ID, got.ID)
	}
	if got.Status != domain.StatusInProgress {
		t.Fatalf("expected INPROG, got %s", got.Status)
	}
	if got.WorkerID != "w1" {
		t.Fatalf("expected worker w1, got %s", got.WorkerID)
	}

	if err := repo.Heartbeat(ctx, got.ID, "w1", 120); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func TestPriorityOrder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	low, _ := repo.Enqueue(ctx, cmd, `{"p":0}`, 0, "", 3, "", time.Time{}, "")
	high, _ := repo.Enqueue(ctx, cmd, `{"p":9}`, 9, "", 3, "", time.Time{}, "")
	mid, _ := repo.Enqueue(ctx, cmd, `{"p":5}`, 5, "", 3, "", time.Time{}, "")

	for i, want := range []*domain.Task{high, mid, low} {
		got, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		if got.ID != want.ID {
			t.Fatalf("claim %d: expected %s, got %s", i, want.ID, got.ID)
		}
	}
}

func TestFIFOWithinPriority(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	a, _ := repo.Enqueue(ctx, cmd, `{"o":"a"}`, 5, "", 3, "", time.Time{}, "")
	b, _ := repo.Enqueue(ctx, cmd, `{"o":"b"}`, 5, "", 3, "", time.Time{}, "")
	c, _ := repo.Enqueue(ctx, cmd, `{"o":"c"}`, 5, "", 3, "", time.Time{}, "")

	for i, want := range []*domain.Task{a, b, c} {
		got, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		if got.ID != want.ID {
			t.Fatalf("claim %d: expected %s (%d), got %s", i, want.ID, i, got.ID)
		}
	}
}

func TestNackDelayedThenReclaim(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	// Make reconcile eager — the test would otherwise wait 500ms.
	repo.reconcile.interval = 0
	cmd := domain.CmdGenerateMaster

	enq, _ := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	claimed, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	delay, dlq, err := repo.Nack(ctx, claimed.ID, "w", 0, 3, "EPHEMERAL")
	if err != nil {
		t.Fatalf("nack: %v", err)
	}
	if dlq {
		t.Fatal("did not expect DLQ on first nack")
	}
	if delay != 0 {
		t.Fatalf("expected delay=0, got %d", delay)
	}

	// MoveDueDelayed surfaces the just-nacked task immediately (visibleAt=now).
	again, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	if again.ID != enq.ID {
		t.Fatalf("expected same id back, got %s vs %s", again.ID, enq.ID)
	}
}

func TestNackEventuallyDLQ(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 1)
	repo.reconcile.interval = 0
	cmd := domain.CmdGenerateMaster

	// Each Claim and each Nack increment attempts. With maxAttempts=3 the
	// sequence is: claim(1)→nack(2,<3)→delayed → claim(3)→nack(4,>=3)→DLQ.
	_, _ = repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, "")
	for i := 0; i < 2; i++ {
		c, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if c == nil {
			t.Fatalf("claim %d: no task", i)
		}
		dly, dlq, err := repo.Nack(ctx, c.ID, "w", 0, 3, "ERR")
		if err != nil {
			t.Fatalf("nack %d: %v", i, err)
		}
		if i == 1 {
			if !dlq {
				t.Fatalf("expected DLQ on second nack cycle, got delay=%d", dly)
			}
		}
	}
}

// TestDelayedCounterFastPath verifies the moveDueDelayedForTenant fast
// path: with zero delayed entries for a (cmd, tenant) the iter is
// skipped entirely. We can't observe the skip directly (it's an internal
// alloc win), but we can pin the externally-visible invariants: counter
// stays at zero with no delayed activity, counter goes up when Nack/
// Enqueue write to delayed, and goes back to zero after a successful
// sweep. A regression in any of these would re-introduce the Phase-0
// alloc hotspot or — worse — leave real delayed tasks stranded.
func TestDelayedCounterFastPath(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	repo.reconcile.interval = 0
	cmd := domain.CmdGenerateMaster

	counter := repo.delayedCounter(cmd, "")
	if got := counter.Load(); got != 0 {
		t.Fatalf("initial counter must be zero, got %d", got)
	}

	// A non-delayed enqueue must not bump the counter (it goes straight
	// to pending).
	if _, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("pending enqueue must not bump delayed counter, got %d", got)
	}

	// A delayed enqueue bumps it.
	if _, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 5, "", 3, "", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("delayed enqueue: %v", err)
	}
	if got := counter.Load(); got != 1 {
		t.Fatalf("delayed enqueue must bump counter to 1, got %d", got)
	}

	// Claim the pending task, then Nack it back with delay=0. Nack
	// always routes through the delayed bucket so the counter must rise.
	claimed, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, _, err := repo.Nack(ctx, claimed.ID, "w", 0, 3, "EPHEMERAL"); err != nil {
		t.Fatalf("nack: %v", err)
	}
	if got := counter.Load(); got != 2 {
		t.Fatalf("after nack counter must be 2, got %d", got)
	}

	// Sweep: a Claim runs moveDueDelayedForTenant which should drain
	// every due-now entry (the Nack one, score=now). The hour-out one
	// stays delayed.
	if _, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, ""); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if got := counter.Load(); got != 1 {
		t.Fatalf("after sweep counter must drop to 1 (the future-delayed one remains), got %d", got)
	}
}

// TestDelayedCounterRecovery: persisted delayed entries from a prior
// process lifecycle must be reflected in the counter after Open so the
// fast-path skip doesn't strand them.
func TestDelayedCounterRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db1, err := Open(Options{Path: dir})
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	repo1 := NewTaskRepository(db1, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	for i := 0; i < 3; i++ {
		if _, err := repo1.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Now().Add(time.Hour), ""); err != nil {
			t.Fatalf("seed enqueue %d: %v", i, err)
		}
	}
	if got := repo1.delayedCounter(cmd, "").Load(); got != 3 {
		t.Fatalf("seed counter must be 3, got %d", got)
	}
	_ = db1.Close()

	db2, err := Open(Options{Path: dir})
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	repo2 := NewTaskRepository(db2, time.UTC, "fixed", 1, 5)
	if got := repo2.delayedCounter(cmd, "").Load(); got != 3 {
		t.Fatalf("recovered counter must be 3, got %d", got)
	}
}

func TestIdempotencyReturnsOriginal(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	a, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "job-7", time.Time{}, "")
	if err != nil {
		t.Fatalf("enq1: %v", err)
	}
	b, err := repo.Enqueue(ctx, cmd, `{"x":2}`, 5, "", 3, "job-7", time.Time{}, "")
	if err != nil {
		t.Fatalf("enq2: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("expected same id for idempotency key, got %s vs %s", a.ID, b.ID)
	}
}

// TestAdminQueuesAggregates is a regression for the nil-int64 panic on
// first sighting of a bucket: out[bucket].(int64) blew up the admin
// endpoint when the map entry didn't exist yet.
func TestAdminQueuesAggregates(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	for range 3 {
		if _, err := repo.Enqueue(ctx, cmd, `{}`, 0, "", 3, "", time.Time{}, ""); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	out, err := repo.AdminQueues(ctx)
	if err != nil {
		t.Fatalf("AdminQueues: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("AdminQueues returned no buckets")
	}
	var total int64
	for _, v := range out {
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("bucket value not int64: %T %v", v, v)
		}
		total += n
	}
	if total != 3 {
		t.Fatalf("total entries=%d, want 3", total)
	}
}
