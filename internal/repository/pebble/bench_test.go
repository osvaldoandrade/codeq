package pebble

import (
	"context"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// BenchmarkClaimNoDelayed reproduces the bench-shape scenario (lots of
// claims, zero delayed entries). Before the fast-path skip, every Claim
// opened a Pebble iter through moveDueDelayedForTenant for nothing —
// Phase 0 profiling pinned this at ~27% of all heap allocs. With the
// counter check this drops to a single atomic load per Claim.
//
// Run: go test -bench BenchmarkClaimNoDelayed -benchmem ./internal/repository/pebble/
func BenchmarkClaimNoDelayed(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	db, err := Open(Options{Path: dir})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	repo.reconcile.interval = time.Hour // disable lease-expiry sweep noise
	cmd := domain.CmdGenerateMaster

	// Pre-fill enough pending tasks to outlast the bench.
	for i := 0; i < b.N+1; i++ {
		if _, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, ""); err != nil {
			b.Fatalf("seed enqueue %d: %v", i, err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil {
			b.Fatalf("claim %d: %v", i, err)
		}
		if t == nil {
			b.Fatalf("claim %d returned no task", i)
		}
	}
}

// BenchmarkEnqueueParallel_Direct bypasses the coalescer by calling
// pebble.Batch.Commit directly. Use it as the control against
// BenchmarkEnqueueParallel to measure the net throughput swing the
// coalescer delivers (positive on -cpu high, neutral or negative on
// -cpu 1 since the coalescer adds one channel hop).
func BenchmarkEnqueueParallel_Direct(b *testing.B) {
	dir := b.TempDir()
	db, err := Open(Options{Path: dir})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			batch := db.Raw().NewBatch()
			if err := batch.Set([]byte("k"), []byte("v"), nil); err != nil {
				b.Fatalf("set: %v", err)
			}
			if err := batch.Commit(pebbledb.NoSync); err != nil {
				b.Fatalf("commit: %v", err)
			}
			_ = batch.Close()
		}
	})
}

// BenchmarkEnqueueParallel measures the throughput win from group commit
// on the hottest write path: many goroutines calling Enqueue concurrently.
// Pre-coalescer, each Enqueue spent the bulk of its time waiting on the
// Pebble commitPipeline mutex (96% of mutex profile in Phase 0). The
// coalescer collapses N concurrent commits into one, so we'd expect this
// benchmark to scale near-linearly with -cpu up to maxMergeBatch.
//
// Run: go test -bench BenchmarkEnqueueParallel -benchmem -cpu 1,4,16 ./internal/repository/pebble/
func BenchmarkEnqueueParallel(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	db, err := Open(Options{Path: dir})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, ""); err != nil {
				b.Fatalf("enqueue: %v", err)
			}
		}
	})
}

// BenchmarkClaimNoDelayed_IterForced is the "as if the fast path didn't
// exist" control: we lie about the counter by bumping it to 1 between
// every Claim, which makes moveDueDelayedForTenant fall through to the
// Pebble iter even though there's nothing to do. Diff against
// BenchmarkClaimNoDelayed shows the raw alloc / wallclock savings the
// fast-path skip delivers.
func BenchmarkClaimNoDelayed_IterForced(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	db, err := Open(Options{Path: dir})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	repo.reconcile.interval = time.Hour
	cmd := domain.CmdGenerateMaster

	for i := 0; i < b.N+1; i++ {
		if _, err := repo.Enqueue(ctx, cmd, `{"x":1}`, 5, "", 3, "", time.Time{}, ""); err != nil {
			b.Fatalf("seed enqueue %d: %v", i, err)
		}
	}

	counter := repo.delayedCounter(cmd, "")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		counter.Store(1) // force iter open even though no real delayed work
		t, _, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 60, 50, 3, "")
		if err != nil {
			b.Fatalf("claim %d: %v", i, err)
		}
		if t == nil {
			b.Fatalf("claim %d returned no task", i)
		}
		// Reset to 0 so the decrement-by-len in the sweep (it would have
		// subtracted len(batchEntries)=0 anyway) doesn't underflow.
		counter.Store(0)
	}
}
