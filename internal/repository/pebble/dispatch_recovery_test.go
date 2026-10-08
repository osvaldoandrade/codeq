package pebble

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestRebuildDispatchRestoresPendingHints(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	first, err := repo.Enqueue(ctx, cmd, `{"n":1}`, 5, "", 3, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := repo.Enqueue(ctx, cmd, `{"n":2}`, 5, "", 3, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	repo.drainHintsLocked()
	if _, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 30, 10, 3, ""); err != nil || ok {
		t.Fatalf("drained queue still claimed: ok=%v err=%v", ok, err)
	}

	repo.dispatchMu.Lock()
	if err := repo.rebuildLocked(); err != nil {
		repo.dispatchMu.Unlock()
		t.Fatalf("rebuild: %v", err)
	}
	repo.dispatchMu.Unlock()

	got := map[string]struct{}{}
	for i := 0; i < 2; i++ {
		task, ok, err := repo.Claim(ctx, "w", []domain.Command{cmd}, 30, 10, 3, "")
		if err != nil || !ok {
			t.Fatalf("claim %d after rebuild: ok=%v err=%v", i, ok, err)
		}
		got[task.ID] = struct{}{}
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, ok := got[id]; !ok {
			t.Fatalf("rebuild did not restore %s; claimed %v", id, got)
		}
	}
}

func TestRequeueExpiredAdoptsLiveLease(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 5, "", 3, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 10, 3, ""); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	db.Leases.Clear()

	if _, err := repo.requeueExpired(ctx, cmd, 10, 3, ""); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if _, ok := db.Leases.Get(task.ID); !ok {
		t.Fatal("live lease was not restored from the task body")
	}
	has, err := db.Has(KeyInprog(cmd, "", task.ID))
	if err != nil || !has {
		t.Fatalf("inprog index missing after adopt: has=%v err=%v", has, err)
	}
	got, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.StatusInProgress {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestRequeueExpiredWithoutMemoryLeaseRequeues(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	task, err := repo.Enqueue(ctx, cmd, `{"k":1}`, 5, "", 3, "", "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok, err := repo.Claim(ctx, "w1", []domain.Command{cmd}, 60, 10, 3, ""); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	body, err := db.Get(KeyTask(task.ID))
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	var stored domain.Task
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stored.LeaseUntil = time.Unix(1, 0).UTC().Format(time.RFC3339)
	updated, err := json.Marshal(&stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Set(KeyTask(task.ID), updated); err != nil {
		t.Fatalf("set: %v", err)
	}
	db.Leases.Clear()

	if _, err := repo.requeueExpired(ctx, cmd, 10, 3, ""); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	got, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status == domain.StatusInProgress {
		t.Fatalf("expired task stayed in progress with no memory lease")
	}
	has, err := db.Has(KeyInprog(cmd, "", task.ID))
	if err != nil {
		t.Fatalf("has inprog: %v", err)
	}
	if has {
		t.Fatal("inprog index survived requeue")
	}
	if _, ok := db.Leases.Get(task.ID); ok {
		t.Fatal("memory lease survived requeue")
	}
}
