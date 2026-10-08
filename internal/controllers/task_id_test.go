package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const sampleTaskID = "ws-1.job-7"

func taskIDRecorder(seen *[]string, err error) *mockSchedulerService {
	return &mockSchedulerService{createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, _ string, _ string, taskID string, _ time.Time, _ int, _ string) (*domain.Task, error) {
		*seen = append(*seen, taskID)
		if err != nil {
			return nil, err
		}
		return &domain.Task{ID: taskID, Command: cmd, Status: domain.StatusPending}, nil
	}}
}

func TestCreateTaskPassesTaskID(t *testing.T) {
	var seen []string
	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyTaskID: sampleTaskID}))
	NewCreateTaskController(taskIDRecorder(&seen, nil)).Handle(ctx)
	var task domain.Task
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &task) != nil || task.ID != sampleTaskID || len(seen) != 1 || seen[0] != sampleTaskID {
		t.Fatalf("status %d body %s seen %q", rec.Code, rec.Body.String(), seen)
	}
}

func TestCreateTaskIDConflictIs409WithoutTask(t *testing.T) {
	var seen []string
	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyTaskID: sampleTaskID}))
	NewCreateTaskController(taskIDRecorder(&seen, domain.ErrTaskIDConflict)).Handle(ctx)
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"task_id_conflict"}` {
		t.Fatalf("status %d body %s, want 409 with only the code", rec.Code, rec.Body.String())
	}
}

func TestCreateTaskInvalidTaskIDIs400(t *testing.T) {
	var seen []string
	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyTaskID: "a/b"}))
	NewCreateTaskController(taskIDRecorder(&seen, domain.ErrInvalidTaskID)).Handle(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestBatchCreatePassesTaskIDPerItem(t *testing.T) {
	var seen []string
	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{keyTasks: []map[string]any{
		{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyTaskID: "a"},
		{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 2},
	}}))
	NewBatchCreateTaskController(taskIDRecorder(&seen, nil)).Handle(ctx)
	if rec.Code != http.StatusOK || len(seen) != 2 || seen[0] != "a" || seen[1] != "" {
		t.Fatalf("status %d seen %q", rec.Code, seen)
	}
}

// TestCreateTaskIDWithDeduplicationIs400 runs the real scheduler service: a
// create that names its task and also asks to deduplicate is refused with 400
// and writes nothing (ADR 0004 and ADR 0008 together).
func TestCreateTaskIDWithDeduplicationIs400(t *testing.T) {
	db, err := pebblerepo.Open(pebblerepo.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := pebblerepo.NewTaskRepository(db, time.UTC, "exp_full_jitter", 1, 10)
	svc := services.NewSchedulerService(repo, nil, nil, time.UTC, time.Now, 60, 10, 3, "exp_full_jitter", 1, 10)

	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{
		keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyTaskID: sampleTaskID, keyDeduplication: dedupeKeyValue,
	}))
	NewCreateTaskController(svc).Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body[errorField] != domain.ErrTaskIDWithDeduplication.Error() {
		t.Fatalf("body %s, want the mutual-exclusion error", rec.Body.String())
	}
	if _, err := repo.Get(context.Background(), sampleTaskID); err == nil {
		t.Fatal("a refused create stored the named task")
	}
}
