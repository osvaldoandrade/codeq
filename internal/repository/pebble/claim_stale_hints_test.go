package pebble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestClaimSkipsExpiredHintsBeforeFreshTask(t *testing.T) {
	for _, reap := range []bool{false, true} {
		t.Run(fmt.Sprintf("reaped=%v", reap), func(t *testing.T) {
			db, repo, _, clock := absoluteFixture(t)
			base := time.Now().UTC()
			clock.Store(base.UnixNano())
			enqueue := func(id string, deadline time.Time) *domain.Task {
				ctx := domain.WithTaskExpiry(context.Background(), deadline)
				task, err := repo.Enqueue(ctx, absoluteTestTopic, `{"batch":1}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a")
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			const expired = 40
			for i := 0; i < expired; i++ {
				enqueue(fmt.Sprintf("old-%d", i), base.Add(time.Minute))
			}
			clock.Store(base.Add(61 * time.Second).UnixNano())
			if reap {
				removed, err := db.sweepAbsolute(context.Background(), 100)
				if err != nil || removed != expired {
					t.Fatalf("reap=%d want=%d err=%v", removed, expired, err)
				}
			}
			fresh := enqueue("fresh", base.Add(121*time.Second))
			got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a")
			if err != nil || !ok || got == nil || got.ID != fresh.ID {
				t.Fatalf("live task behind expired hints was not claimed: task=%+v ok=%v err=%v", got, ok, err)
			}
			if got.Status != domain.StatusInProgress || !got.ExpiresAt.Equal(*fresh.ExpiresAt) {
				t.Fatalf("claim changed task lifetime or status: %+v", got)
			}
			if hints := len(repo.channelFor(absoluteTestTopic, "tenant-a", 3).ch); hints != 0 {
				t.Fatalf("remaining hints=%d", hints)
			}
		})
	}
}

func enqueueClaimHint(t *testing.T, repo *TaskRepository, cmd domain.Command, tenant string, priority int) *domain.Task {
	t.Helper()
	task, err := repo.Enqueue(context.Background(), cmd, `{}`, priority, "", 3, "", "", "", time.Time{}, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func makeClaimHintGhost(t *testing.T, db *DB, task *domain.Task) {
	t.Helper()
	if err := db.Delete(KeyTask(task.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestClaimInspectionBudgetAcrossPrioritiesAndCommands(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	const first, second domain.Command = "first", "second"
	for _, priority := range []int{9, 9, 8, 8} {
		makeClaimHintGhost(t, db, enqueueClaimHint(t, repo, first, "tenant-a", priority))
	}
	want := enqueueClaimHint(t, repo, first, "tenant-a", 7)
	nextCommand := enqueueClaimHint(t, repo, second, "tenant-a", 9)
	otherTenant := enqueueClaimHint(t, repo, first, "tenant-b", 9)
	commands := []domain.Command{first, second}
	if got, ok, err := repo.Claim(context.Background(), "worker", commands, 60, 3, 3, "tenant-a"); err != nil || ok || got != nil {
		t.Fatalf("budget exhausted claim=%+v ok=%v err=%v", got, ok, err)
	}
	if hints := len(repo.channelFor(first, "tenant-a", 8).ch); hints != 1 {
		t.Fatalf("budget did not stop after three hints: remaining priority-8 hints=%d", hints)
	}
	for _, task := range []*domain.Task{want, nextCommand, otherTenant} {
		stored, err := repo.Get(context.Background(), task.ID)
		if err != nil || stored.Status != domain.StatusPending {
			t.Fatalf("uninspected task changed: task=%+v err=%v", stored, err)
		}
	}
	got, ok, err := repo.Claim(context.Background(), "worker", commands, 60, 3, 3, "tenant-a")
	if err != nil || !ok || got == nil || got.ID != want.ID {
		t.Fatalf("next claim did not preserve priority/command/tenant: task=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestClaimInspectionBudgetCountsReservedHints(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	const cmd domain.Command = "cf-logs"
	for i := 0; i < 3; i++ {
		task := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
		if !repo.tryReserve(task.ID) {
			t.Fatal("reserve")
		}
		t.Cleanup(func() { repo.release(task.ID) })
	}
	fresh := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
	if got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 2, 3, "tenant-a"); err != nil || ok || got != nil {
		t.Fatalf("reserved hints bypassed budget: task=%+v ok=%v err=%v", got, ok, err)
	}
	if hints := len(repo.channelFor(cmd, "tenant-a", 3).ch); hints != 2 {
		t.Fatalf("remaining hints=%d want=2", hints)
	}
	got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 2, 3, "tenant-a")
	if err != nil || !ok || got == nil || got.ID != fresh.ID {
		t.Fatalf("next claim=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestClaimSkipsMalformedAndNonPendingHints(t *testing.T) {
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	const cmd domain.Command = "cf-logs"
	broken := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
	if err := db.Set(KeyTask(broken.ID), []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	owned := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
	owned.Status, owned.WorkerID = domain.StatusInProgress, "other-worker"
	body, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set(KeyTask(owned.ID), body); err != nil {
		t.Fatal(err)
	}
	fresh := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
	got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 0, 3, "tenant-a")
	if err != nil || !ok || got == nil || got.ID != fresh.ID {
		t.Fatalf("claim after stale hints=%+v ok=%v err=%v", got, ok, err)
	}
	stored, err := repo.Get(context.Background(), owned.ID)
	if err != nil || stored.WorkerID != "other-worker" || stored.Status != domain.StatusInProgress {
		t.Fatalf("stole existing assignment: %+v err=%v", stored, err)
	}
}

type cancelAfterClaimWrite struct {
	*switchReplicator
	cancel context.CancelFunc
}

func (r *cancelAfterClaimWrite) Replicate(repr []byte) error {
	err := r.switchReplicator.Replicate(repr)
	if err == nil && r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return err
}

func TestClaimInspectionHonorsCancellation(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(fmt.Sprintf("during=%v", during), func(t *testing.T) {
			db := openTestDB(t)
			repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
			const cmd domain.Command = "cf-logs"
			makeClaimHintGhost(t, db, enqueueClaimHint(t, repo, cmd, "tenant-a", 3))
			fresh := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if during {
				db.AttachReplicator(&cancelAfterClaimWrite{switchReplicator: &switchReplicator{db: db.Raw()}, cancel: cancel})
			} else {
				cancel()
			}
			got, ok, err := repo.Claim(ctx, "worker", []domain.Command{cmd}, 60, 100, 3, "tenant-a")
			if !errors.Is(err, context.Canceled) || ok || got != nil {
				t.Fatalf("canceled claim=%+v ok=%v err=%v", got, ok, err)
			}
			stored, err := repo.Get(context.Background(), fresh.ID)
			if err != nil || stored.Status != domain.StatusPending {
				t.Fatalf("cancellation consumed fresh task=%+v err=%v", stored, err)
			}
			got, ok, err = repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 100, 3, "tenant-a")
			if err != nil || !ok || got == nil || got.ID != fresh.ID {
				t.Fatalf("retry lost fresh task=%+v ok=%v err=%v", got, ok, err)
			}
		})
	}
}

func TestClaimInspectionRepublishesHintOnWriteError(t *testing.T) {
	for _, ghost := range []bool{false, true} {
		t.Run(fmt.Sprintf("ghost=%v", ghost), func(t *testing.T) {
			db := openTestDB(t)
			repl := &switchReplicator{db: db.Raw()}
			repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
			const cmd domain.Command = "cf-logs"
			if ghost {
				makeClaimHintGhost(t, db, enqueueClaimHint(t, repo, cmd, "tenant-a", 3))
			}
			fresh := enqueueClaimHint(t, repo, cmd, "tenant-a", 3)
			db.AttachReplicator(repl)
			repl.fail.Store(true)
			got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 100, 3, "tenant-a")
			if err == nil || ok || got != nil {
				t.Fatalf("replication error hidden: task=%+v ok=%v err=%v", got, ok, err)
			}
			wantHints := 1
			if ghost {
				wantHints++
			}
			if hints := len(repo.channelFor(cmd, "tenant-a", 3).ch); hints != wantHints {
				t.Fatalf("failed write lost hint: got=%d want=%d", hints, wantHints)
			}
			repl.fail.Store(false)
			got, ok, err = repo.Claim(context.Background(), "worker", []domain.Command{cmd}, 60, 100, 3, "tenant-a")
			if err != nil || !ok || got == nil || got.ID != fresh.ID {
				t.Fatalf("retry lost fresh task=%+v ok=%v err=%v", got, ok, err)
			}
		})
	}
}
