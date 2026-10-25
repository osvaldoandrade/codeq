package pebble

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/storage/diskguard"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const admissionCmd = domain.Command("storage-admission")

// switchAdmission is a test admission hook whose verdict the test flips.
type switchAdmission struct{ blocked atomic.Bool }

func (s *switchAdmission) admit() error {
	if s.blocked.Load() {
		return domain.ErrStorageCapacity
	}
	return nil
}

func TestStorageAdmissionRefusesNewTasksButKeepsDrainPaths(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	gate := &switchAdmission{}
	db.SetCreateAdmission(gate.admit)
	tasks := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	results := NewResultRepository(db, time.UTC)

	first, err := tasks.Enqueue(ctx, admissionCmd, `{"n":1}`, 0, "", 3, "idem-1", "", "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue before pressure: %v", err)
	}
	second, err := tasks.Enqueue(ctx, admissionCmd, `{"n":2}`, 0, "", 3, "", "", "named-2", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("named enqueue before pressure: %v", err)
	}

	gate.blocked.Store(true)

	// New work is refused on every create shape with the stable sentinel.
	for name, create := range map[string]func() error{
		"fresh": func() error {
			_, err := tasks.Enqueue(ctx, admissionCmd, `{"n":3}`, 0, "", 3, "", "", "", time.Time{}, "tenant-a")
			return err
		},
		"new idempotency key": func() error {
			_, err := tasks.Enqueue(ctx, admissionCmd, `{"n":4}`, 0, "", 3, "idem-new", "", "", time.Time{}, "tenant-a")
			return err
		},
		"new client task id": func() error {
			_, err := tasks.Enqueue(ctx, admissionCmd, `{"n":5}`, 0, "", 3, "", "", "named-new", time.Time{}, "tenant-a")
			return err
		},
		"delayed create": func() error {
			_, err := tasks.Enqueue(ctx, admissionCmd, `{"n":6}`, 0, "", 3, "", "", "", time.Now().Add(time.Hour), "tenant-a")
			return err
		},
	} {
		if err := create(); !errors.Is(err, domain.ErrStorageCapacity) {
			t.Fatalf("%s: err = %v, want ErrStorageCapacity", name, err)
		}
	}

	// Replays write nothing new and keep their receipts.
	replay, err := tasks.Enqueue(ctx, admissionCmd, `{"n":1}`, 0, "", 3, "idem-1", "", "", time.Time{}, "tenant-a")
	if err != nil || replay.ID != first.ID {
		t.Fatalf("idempotent replay under pressure: %v %v", replay, err)
	}
	named, err := tasks.Enqueue(ctx, admissionCmd, `{"n":2}`, 0, "", 3, "", "", "named-2", time.Time{}, "tenant-a")
	if err != nil || named.ID != second.ID {
		t.Fatalf("named replay under pressure: %v %v", named, err)
	}

	// Drain paths keep working: claim, heartbeat, complete, nack, cleanup.
	claimed, ok, err := tasks.Claim(ctx, "w1", []domain.Command{admissionCmd}, 60, 50, 3, "tenant-a")
	if err != nil || !ok {
		t.Fatalf("claim under pressure: ok=%v err=%v", ok, err)
	}
	if err := tasks.Heartbeat(ctx, claimed.ID, "w1", 120); err != nil {
		t.Fatalf("heartbeat under pressure: %v", err)
	}
	completeTask(t, results, claimed)
	other, ok, err := tasks.Claim(ctx, "w2", []domain.Command{admissionCmd}, 60, 50, 3, "tenant-a")
	if err != nil || !ok {
		t.Fatalf("second claim under pressure: ok=%v err=%v", ok, err)
	}
	if _, _, err := tasks.Nack(ctx, other.ID, "w2", 0, 3, "retry"); err != nil {
		t.Fatalf("nack under pressure: %v", err)
	}
	if _, err := tasks.CleanupExpired(ctx, 100, time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatalf("cleanup under pressure: %v", err)
	}

	gate.blocked.Store(false)
	if _, err := tasks.Enqueue(ctx, admissionCmd, `{"n":7}`, 0, "", 3, "", "", "", time.Time{}, "tenant-a"); err != nil {
		t.Fatalf("enqueue after recovery: %v", err)
	}
}

func completeTask(t *testing.T, results *ResultRepository, task *domain.Task) {
	t.Helper()
	ctx := context.Background()
	rec := domain.ResultRecord{TaskID: task.ID, Status: domain.StatusCompleted, Result: map[string]any{"ok": true}, CompletedAt: time.Now()}
	if err := results.SaveResult(ctx, rec, task.Command, task.TenantID); err != nil {
		t.Fatalf("save result under pressure: %v", err)
	}
	if err := results.UpdateTaskOnComplete(ctx, task.ID, task.Command, task.TenantID, domain.StatusCompleted, ""); err != nil {
		t.Fatalf("complete under pressure: %v", err)
	}
	if err := results.RemoveFromInprogAndClearLease(ctx, task.ID, task.Command, task.TenantID); err != nil {
		t.Fatalf("clear lease under pressure: %v", err)
	}
}

// TestStorageAdmissionSimulatedVolumeReachesThreshold drives the real guard
// with a simulated statfs over the Pebble directory: a 16 MiB volume whose
// used bytes are the bytes Pebble actually wrote. Creates are refused once
// the volume crosses 80%, while claims keep draining the queue.
func TestStorageAdmissionSimulatedVolumeReachesThreshold(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(Options{Path: dir, MemTableBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const total = 16 << 20
	stat := func(string) (diskguard.Usage, error) {
		used := dirBytes(t, dir)
		if used > total {
			used = total
		}
		return diskguard.Usage{TotalBytes: total, FreeBytes: total - used}, nil
	}
	guard, err := diskguard.New(diskguard.Config{Path: dir, MaxUsedFraction: 0.80, Stat: stat, Interval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	db.SetCreateAdmission(guard.AdmitCreate)
	tasks := NewTaskRepository(db, time.UTC, "fixed", 1, 5)

	accepted := 0
	var refusal error
	for range 1000 {
		// Random hex does not compress away in sstables, so the simulated
		// volume fills like a real one would.
		blob := make([]byte, 32<<10)
		if _, err := rand.Read(blob); err != nil {
			t.Fatal(err)
		}
		payload := `{"blob":"` + hex.EncodeToString(blob) + `"}`
		if _, err := tasks.Enqueue(ctx, admissionCmd, payload, 0, "", 3, "", "", "", time.Time{}, "tenant-a"); err != nil {
			refusal = err
			break
		}
		accepted++
	}
	if !errors.Is(refusal, domain.ErrStorageCapacity) {
		t.Fatalf("volume never reached admission: accepted=%d err=%v", accepted, refusal)
	}
	if accepted == 0 {
		t.Fatal("guard refused before the volume filled")
	}
	if u, _ := guard.Refresh(); u.UsedFraction() < 0.80 {
		t.Fatalf("refused at %.2f used, below the threshold", u.UsedFraction())
	}
	if _, ok, err := tasks.Claim(ctx, "w1", []domain.Command{admissionCmd}, 60, 50, 3, "tenant-a"); err != nil || !ok {
		t.Fatalf("claim on a full volume: ok=%v err=%v", ok, err)
	}
}

func dirBytes(t *testing.T, dir string) uint64 {
	t.Helper()
	var n uint64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // Pebble removed an obsolete file mid-walk.
			}
			return err
		}
		n += uint64(info.Size()) // #nosec G115 -- file sizes are non-negative
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}
