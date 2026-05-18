package pebble

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// setupExpiredLease enqueues a task, claims it, then backdates its
// lease entry so the reaper considers it expired.
func setupExpiredLease(t *testing.T, db *DB, repo *TaskRepository, cmd domain.Command) string {
	t.Helper()
	ctx := context.Background()
	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 0, "", 1, "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, _, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 30, 1, 1, ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	db.Leases.Set(task.ID, "w1", cmd, "", 1) // far past
	return task.ID
}

// TestReaper_LeaderGate_SkipsTickWhenNotLeader is the raft-follower
// case: a non-nil gate that returns false ⇒ the reaper does NOT sweep,
// even with expired leases on disk. Followers stay passive.
func TestReaper_LeaderGate_SkipsTickWhenNotLeader(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	id := setupExpiredLease(t, db, repo, cmd)

	gateCalls := atomic.Int32{}
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{
		LeaseInterval:      20 * time.Millisecond,
		TTLInterval:        time.Hour,
		MaxAttemptsDefault: 1,
		LeaderGate: func() bool {
			gateCalls.Add(1)
			return false
		},
	})
	go r.Start(subCtx)

	// Let several ticks elapse. With gate=false the reaper should
	// never call sweepLeases — task stays IN_PROGRESS.
	time.Sleep(150 * time.Millisecond)
	cancel()

	got, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.StatusInProgress {
		t.Errorf("follower reaper reaped despite gate=false: status=%s loc=%s",
			got.Status, got.LastKnownLocation)
	}
	if gateCalls.Load() < 2 {
		t.Errorf("gate calls: want ≥2 ticks polled, got %d", gateCalls.Load())
	}
}

// TestReaper_LeaderGate_RunsTickWhenLeader is the leader case: gate
// returns true ⇒ reaper sweeps and the expired lease lands in DLQ.
func TestReaper_LeaderGate_RunsTickWhenLeader(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	id := setupExpiredLease(t, db, repo, cmd)

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{
		LeaseInterval:      20 * time.Millisecond,
		TTLInterval:        time.Hour,
		MaxAttemptsDefault: 1,
		LeaderGate:         func() bool { return true },
	})
	go r.Start(subCtx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := repo.Get(context.Background(), id)
		if err == nil && got.LastKnownLocation == domain.LocationDLQ {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper with leader=true never moved task to DLQ")
}

// TestReaper_NilGate_RunsAlways: backward-compat path. LeaderGate=nil
// preserves the historical single-node behavior — every tick runs.
func TestReaper_NilGate_RunsAlways(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster
	id := setupExpiredLease(t, db, repo, cmd)

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{
		LeaseInterval:      20 * time.Millisecond,
		TTLInterval:        time.Hour,
		MaxAttemptsDefault: 1,
		// LeaderGate intentionally nil
	})
	go r.Start(subCtx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := repo.Get(context.Background(), id)
		if err == nil && got.LastKnownLocation == domain.LocationDLQ {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reaper with nil gate never moved task to DLQ")
}
