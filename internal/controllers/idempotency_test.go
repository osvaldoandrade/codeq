package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	keyIdempotency   = "idempotencyKey"
	clientKey        = "order-42"
	conflictBody     = `{"error":"idempotency_conflict"}`
	secretPayload    = "secret"
	namespacedClient = bindingTenant + "\x00" + topicA + "\x00" + clientKey
)

// recordingScheduler captures the storage idempotency key and answers with
// the configured task or error.
func recordingScheduler(seen *string, task *domain.Task, err error) *mockSchedulerService {
	return &mockSchedulerService{createFunc: func(_ context.Context, _ domain.Command, _ string, _ int, _ string, _ int, key string, _ string, _ time.Time, _ int, _ string) (*domain.Task, error) {
		*seen = key
		return task, err
	}}
}

func TestStorageIdempotencyKeyFormat(t *testing.T) {
	scope := publishScope()
	if got := storageIdempotencyKey(scope, clientKey); got != namespacedClient {
		t.Fatalf("binding key = %q, want %q", got, namespacedClient)
	}
	other := publishScope()
	other.EventType = topicB
	if storageIdempotencyKey(scope, clientKey) == storageIdempotencyKey(other, clientKey) {
		t.Fatal("two topics of one tenant share a namespace")
	}
	if got := storageIdempotencyKey(nil, clientKey); got != clientKey {
		t.Fatalf("legacy key = %q, want unchanged", got)
	}
	if got := storageIdempotencyKey(scope, ""); got != "" {
		t.Fatalf("empty key = %q, want empty", got)
	}
}

func TestCreateTaskNamespacesBindingKeyOnly(t *testing.T) {
	var seen string
	body := map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: clientKey}
	c, rec := newTestContext(t, jsonBody(t, body))
	withScope(c, publishScope())
	NewCreateTaskController(recordingScheduler(&seen, ownTask("t"), nil)).Handle(c)
	if rec.Code != http.StatusAccepted || seen != namespacedClient {
		t.Fatalf("binding: status %d key %q", rec.Code, seen)
	}
	c, rec = newTestContext(t, jsonBody(t, body))
	c.Set("tenantID", bindingTenant)
	NewCreateTaskController(recordingScheduler(&seen, ownTask("t"), nil)).Handle(c)
	if rec.Code != http.StatusAccepted || seen != clientKey {
		t.Fatalf("legacy: status %d key %q", rec.Code, seen)
	}
}

func TestCreateTaskRejectsNULInIdempotencyKey(t *testing.T) {
	for name, binding := range map[string]bool{"binding": true, "legacy": false} {
		called := ""
		c, rec := newTestContext(t, jsonBody(t, map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: bindingTenant + "\x00" + topicA + "\x00k"}))
		if binding {
			withScope(c, publishScope())
		}
		NewCreateTaskController(recordingScheduler(&called, ownTask("t"), nil)).Handle(c)
		if rec.Code != http.StatusBadRequest || called != "" || !bodyHasError(t, rec, errInvalidIdempotency) {
			t.Fatalf("%s: status %d called %q body %s", name, rec.Code, called, rec.Body.String())
		}
	}
}

func TestCreateTaskIdempotencyConflictHasNoBody(t *testing.T) {
	cases := map[string]struct {
		binding bool
		task    *domain.Task
		err     error
	}{
		"repository conflict, legacy":  {false, nil, domain.ErrIdempotencyConflict},
		"repository conflict, binding": {true, nil, domain.ErrIdempotencyConflict},
		"binding got other topic":      {true, &domain.Task{ID: "t", TenantID: bindingTenant, Command: topicB, Payload: secretPayload}, nil},
		"binding got other tenant":     {true, &domain.Task{ID: "t", TenantID: otherTenant, Command: topicA, Payload: secretPayload}, nil},
	}
	for name, tc := range cases {
		var seen string
		c, rec := newTestContext(t, jsonBody(t, map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: clientKey}))
		if tc.binding {
			withScope(c, publishScope())
		} else {
			c.Set("tenantID", otherTenant)
		}
		NewCreateTaskController(recordingScheduler(&seen, tc.task, tc.err)).Handle(c)
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != conflictBody {
			t.Fatalf("%s: status %d body %s, want 409 %s", name, rec.Code, rec.Body.String(), conflictBody)
		}
	}
}

func TestBatchCreateIdempotency(t *testing.T) {
	keys := []string{}
	svc := &mockSchedulerService{createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, key string, _ string, _ time.Time, _ int, tenant string) (*domain.Task, error) {
		keys = append(keys, key)
		if key == bindingTenant+"\x00"+topicA+"\x00taken" {
			return nil, domain.ErrIdempotencyConflict
		}
		return &domain.Task{ID: "t-" + key, Command: cmd, TenantID: tenant, Payload: secretPayload}, nil
	}}
	body := map[string]any{keyTasks: []any{
		map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: "fresh"},
		map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: "taken"},
		map[string]any{keyCommand: topicA, keyPayload: 1, keyIdempotency: "bad\x00key"},
		map[string]any{keyCommand: topicA, keyPayload: 1},
	}}
	c, rec := newTestContext(t, jsonBody(t, body))
	withScope(c, publishScope())
	NewBatchCreateTaskController(svc).Handle(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Results) != 4 {
		t.Fatalf("results: %v %s", err, rec.Body.String())
	}
	if out.Results[0]["task"] == nil {
		t.Fatalf("fresh key not created: %v", out.Results[0])
	}
	if out.Results[1]["error"] != codeIdempotencyConflict || out.Results[1]["task"] != nil {
		t.Fatalf("taken key: %v, want idempotency_conflict and no task", out.Results[1])
	}
	if out.Results[2]["error"] != errInvalidIdempotency || out.Results[2]["task"] != nil {
		t.Fatalf("NUL key: %v", out.Results[2])
	}
	want := []string{bindingTenant + "\x00" + topicA + "\x00fresh", bindingTenant + "\x00" + topicA + "\x00taken", ""}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("storage keys = %q, want %q", keys, want)
	}
}
