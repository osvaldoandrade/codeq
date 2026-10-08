package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	keyDeduplication = "deduplicationKey"
	dedupeKeyValue   = "sync-1"
)

// dedupeRecorder records the deduplication key of every create it serves.
func dedupeRecorder(seen *[]string, err error) *mockSchedulerService {
	return &mockSchedulerService{createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, _ string, key string, _ time.Time, _ int, _ string) (*domain.Task, error) {
		*seen = append(*seen, key)
		if err != nil {
			return nil, err
		}
		return &domain.Task{ID: "task-" + key, Command: cmd, Status: domain.StatusPending, DeduplicationKey: key}, nil
	}}
}

func TestCreateTaskPassesDeduplicationKey(t *testing.T) {
	var seen []string
	ctrl := NewCreateTaskController(dedupeRecorder(&seen, nil))

	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{
		keyCommand: string(domain.CmdGenerateMaster), keyPayload: map[string]int{"n": 1}, keyDeduplication: dedupeKeyValue,
	}))
	ctrl.Handle(ctx)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if len(seen) != 1 || seen[0] != dedupeKeyValue {
		t.Fatalf("service saw keys %q, want [%s]", seen, dedupeKeyValue)
	}
	var task domain.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || task.DeduplicationKey != dedupeKeyValue {
		t.Fatalf("response task %+v (%v) does not carry the key", task, err)
	}
}

func TestBatchCreatePassesDeduplicationKeyPerItem(t *testing.T) {
	var seen []string
	ctrl := NewBatchCreateTaskController(dedupeRecorder(&seen, nil))

	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{
		keyTasks: []map[string]any{
			{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, keyDeduplication: "a"},
			{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 2},
			{keyCommand: string(domain.CmdGenerateMaster), keyPayload: 3, keyDeduplication: "b"},
		},
	}))
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if want := []string{"a", "", "b"}; len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("service saw keys %q, want %q", seen, want)
	}
}

func TestCreateTaskDeduplicationWithIdempotencyIsBadRequest(t *testing.T) {
	var seen []string
	ctrl := NewCreateTaskController(dedupeRecorder(&seen, domain.ErrDeduplicationWithIdempotency))

	ctx, rec := newTestContext(t, jsonBody(t, map[string]any{
		keyCommand: string(domain.CmdGenerateMaster), keyPayload: 1, "idempotencyKey": "order-1", keyDeduplication: dedupeKeyValue,
	}))
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != domain.ErrDeduplicationWithIdempotency.Error() {
		t.Fatalf("body %s, want the mutual-exclusion error", rec.Body.String())
	}
}
