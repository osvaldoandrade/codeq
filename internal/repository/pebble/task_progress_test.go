package pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	progressWorker = "w1"
	progressOther  = "w2"
)

// claimForProgress enqueues one task and claims it for workerID.
func claimForProgress(t *testing.T, repo repository.TaskRepository, workerID string) *domain.Task {
	t.Helper()
	ctx := context.Background()
	enq, err := repo.Enqueue(ctx, domain.CmdGenerateMaster, `{}`, 5, "", 3, "", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, ok, err := repo.Claim(ctx, workerID, []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, "")
	if err != nil || !ok || got.ID != enq.ID {
		t.Fatalf("claim: ok=%v err=%v got=%v want=%s", ok, err, got, enq.ID)
	}
	return got
}

func wantProgress(t *testing.T, task *domain.Task, want string) {
	t.Helper()
	if string(task.Progress) != want {
		t.Fatalf("progress = %q, want %q", task.Progress, want)
	}
}

func TestProgressLeaseHolderWritesAndKeepsItAcrossNack(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	task := claimForProgress(t, repo, progressWorker)

	raw, err := repo.db.Get(KeyTask(task.ID))
	if err != nil || strings.Contains(string(raw), `"progress"`) {
		t.Fatalf("task body before any report must omit progress: %s (err %v)", raw, err)
	}
	if err := repo.Progress(ctx, task.ID, progressWorker, json.RawMessage(`{"done":3,"total":10}`)); err != nil {
		t.Fatalf("owner progress: %v", err)
	}
	got, _ := repo.Get(ctx, task.ID)
	wantProgress(t, got, `{"done":3,"total":10}`)
	if got.Status != domain.StatusInProgress || got.WorkerID != progressWorker {
		t.Fatalf("progress changed lease state: %+v", got)
	}

	if err := repo.Progress(ctx, task.ID, progressOther, json.RawMessage(`99`)); err == nil || err.Error() != "not-owner" {
		t.Fatalf("other worker: err %v, want not-owner", err)
	}
	if _, _, err := repo.Nack(ctx, task.ID, progressWorker, 0, 3, "retry"); err != nil {
		t.Fatalf("nack: %v", err)
	}
	got, _ = repo.Get(ctx, task.ID)
	wantProgress(t, got, `{"done":3,"total":10}`)
	// A pending task has no worker, so an empty caller passes the owner
	// comparison and must still be refused by the state check.
	if err := repo.Progress(ctx, task.ID, "", json.RawMessage(`1`)); err == nil || err.Error() != "not-in-progress" {
		t.Fatalf("pending task: err %v, want not-in-progress", err)
	}

	again, ok, err := repo.Claim(ctx, progressOther, []domain.Command{domain.CmdGenerateMaster}, 60, 50, 3, "")
	if err != nil || !ok || again.ID != task.ID {
		t.Fatalf("re-claim: ok=%v err=%v", ok, err)
	}
	wantProgress(t, again, `{"done":3,"total":10}`)
	if err := repo.Progress(ctx, task.ID, progressOther, json.RawMessage(`"half"`)); err != nil {
		t.Fatalf("new owner progress: %v", err)
	}
	got, _ = repo.Get(ctx, task.ID)
	wantProgress(t, got, `"half"`)
}

func TestProgressUnknownTask(t *testing.T) {
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	if err := repo.Progress(context.Background(), "missing", progressWorker, json.RawMessage(`1`)); err == nil || err.Error() != "not-found" {
		t.Fatalf("err %v, want not-found", err)
	}
}

func TestShardedProgressRoutesToOwningShard(t *testing.T) {
	ctx := context.Background()
	shards := make([]*TaskRepository, 4)
	for i := range shards {
		shards[i] = NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	}
	sharded := NewShardedTaskRepository(shards)
	for i := range 8 {
		task := claimForProgress(t, sharded, progressWorker)
		value := json.RawMessage(fmt.Sprintf(`{"step":%d}`, i))
		if err := sharded.Progress(ctx, task.ID, progressWorker, value); err != nil {
			t.Fatalf("sharded progress: %v", err)
		}
		got, err := shards[sharded.shardOf(task.ID)].Get(ctx, task.ID)
		if err != nil {
			t.Fatalf("owning shard get: %v", err)
		}
		wantProgress(t, got, string(value))
	}
	if err := sharded.Progress(ctx, "missing", progressWorker, json.RawMessage(`1`)); err == nil || err.Error() != "not-found" {
		t.Fatalf("sharded unknown: err %v, want not-found", err)
	}
}
