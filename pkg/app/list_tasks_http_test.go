package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const listTasksCommand = "SYNC_CHANNEL"

func newListTasksServer(t *testing.T, role string) *httptest.Server {
	t.Helper()
	cfg := newTopicPebbleConfig(t, t.TempDir()+"/pebble")
	cfg.ProducerAuthConfig = json.RawMessage(`{"token":"dev-token","subject":"producer-dev","raw":{"role":"` + role + `","tenantId":"dev-tenant"}}`)
	a, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() {
		srv.Close()
		_ = a.TracingShutdown(context.Background())
	})
	return srv
}

func TestListTasksOverHTTP(t *testing.T) {
	srv := newListTasksServer(t, "ADMIN")
	ctx := context.Background()
	created := map[string]bool{}
	for i := range 5 {
		var task domain.Task
		body := map[string]any{keyCommand: listTasksCommand, keyPayload: map[string]int{"n": i}, "priority": i}
		if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, &task); status != http.StatusAccepted {
			t.Fatalf("create %d: %d %s", i, status, raw)
		}
		created[task.ID] = true
	}

	listed := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("listing never ended")
		}
		q := url.Values{"state": {"ready"}, "limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page domain.TaskPage
		status, raw := doJSON(t, ctx, http.MethodGet, srv.URL+"/v1/codeq/admin/queues/"+listTasksCommand+"/tasks?"+q.Encode(), "dev-token", nil, &page)
		if status != http.StatusOK {
			t.Fatalf("page %d: %d %s", pages, status, raw)
		}
		for _, task := range page.Tasks {
			if !created[task.ID] || listed[task.ID] || task.Payload == "" {
				t.Fatalf("page %d: unexpected task %+v", pages, task)
			}
			listed[task.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(listed) != len(created) {
		t.Fatalf("listed %d of %d tasks", len(listed), len(created))
	}

	if status, _ := doJSON(t, ctx, http.MethodGet, srv.URL+"/v1/codeq/admin/queues/"+listTasksCommand+"/tasks?state=done", "dev-token", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("unknown state: status %d, want 400", status)
	}
}

func TestListTasksRequiresAdmin(t *testing.T) {
	srv := newListTasksServer(t, "USER")
	status, raw := doJSON(t, context.Background(), http.MethodGet, srv.URL+"/v1/codeq/admin/queues/"+listTasksCommand+"/tasks?state=ready", "dev-token", nil, nil)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("non-admin listing: %d %s; want 401 or 403", status, raw)
	}
}
