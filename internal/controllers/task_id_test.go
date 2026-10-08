package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const sampleTaskID = "ws-1.job-7"

func taskIDRecorder(seen *[]string, err error) *mockSchedulerService {
	return &mockSchedulerService{createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, _ string, taskID string, _ time.Time, _ int, _ string) (*domain.Task, error) {
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
