package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestLongPollGetResult_E2E exercises the `?waitSeconds=N` long-poll
// added to `GET /v1/codeq/tasks/:id/result`. Three cases:
//   - waiter receives the result mid-flight, before the deadline
//   - waiter times out cleanly with a 404 when the result never lands
//   - missing task returns 404 immediately even with waitSeconds set
func TestLongPollGetResult_E2E(t *testing.T) {
	server := startLongPollServer(t)

	t.Run("waiter receives result mid-flight", func(t *testing.T) {
		id := createTaskNoHook(t, server.URL)
		claimTaskByID(t, server.URL, id)

		// Submit the result 300 ms after starting the long-poll.
		go func() {
			time.Sleep(300 * time.Millisecond)
			submitBody := `{"status":"COMPLETED","result":{"ok":true}}`
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/"+id+"/result", strings.NewReader(submitBody))
			req.Header.Set("Authorization", "Bearer dev-token")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("background submit: %v", err)
				return
			}
			resp.Body.Close()
		}()

		start := time.Now()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/codeq/tasks/"+id+"/result?waitSeconds=5", nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("long-poll GET: %v", err)
		}
		defer resp.Body.Close()
		elapsed := time.Since(start)

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("long-poll: expected 200, got %d body=%s", resp.StatusCode, string(b))
		}
		if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
			t.Errorf("long-poll elapsed=%v, want ~300ms-1s (submit fires at 300ms, poll interval 100ms)", elapsed)
		}

		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		rec, _ := out["result"].(map[string]any)
		if rec == nil || rec["status"] != "COMPLETED" {
			t.Errorf("unexpected result payload: %+v", out)
		}
	})

	t.Run("waiter times out when result never arrives", func(t *testing.T) {
		id := createTaskNoHook(t, server.URL)
		claimTaskByID(t, server.URL, id) // task is IN_PROGRESS, no result.

		start := time.Now()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/codeq/tasks/"+id+"/result?waitSeconds=1", nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("long-poll GET: %v", err)
		}
		defer resp.Body.Close()
		elapsed := time.Since(start)

		if resp.StatusCode != http.StatusNotFound {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 404 on timeout, got %d body=%s", resp.StatusCode, string(b))
		}
		if elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
			t.Errorf("timeout elapsed=%v, want ~1s", elapsed)
		}
	})

	t.Run("missing task short-circuits long-poll", func(t *testing.T) {
		start := time.Now()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/codeq/tasks/does-not-exist/result?waitSeconds=10", nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("long-poll GET: %v", err)
		}
		defer resp.Body.Close()
		elapsed := time.Since(start)

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404, got %d", resp.StatusCode)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("missing-task call took %v — should short-circuit, not wait 10s", elapsed)
		}
	})
}

func startLongPollServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	pcfg, _ := json.Marshal(map[string]any{"path": dir})

	cfg := &config.Config{
		Port:                               0,
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		Env:                                "dev",
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  t.TempDir(),
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      "fixed",
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WebhookHmacSecret:                  "secret",
		WorkerAudience:                     "codeq-worker",
		SubscriptionMinIntervalSeconds:     5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:           1,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
		ProducerAuthProvider:               "static",
		ProducerAuthConfig:                 json.RawMessage(`{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN","tenantId":"dev-tenant"}}`),
		WorkerAuthProvider:                 "static",
		WorkerAuthConfig:                   json.RawMessage(`{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"],"raw":{"tenantId":"dev-tenant"}}`),
		PersistenceProvider:                "pebble",
		PersistenceConfig:                  pcfg,
		RedisAddr:                          "127.0.0.1:0",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	t.Cleanup(func() {
		if app.TracingShutdown != nil {
			_ = app.TracingShutdown(context.Background())
		}
	})
	SetupMappings(app)
	srv := httptest.NewServer(app.Engine)
	t.Cleanup(srv.Close)
	return srv
}

func createTaskNoHook(t *testing.T, base string) string {
	t.Helper()
	body := `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create task: expected 202, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create task: no id in response: %+v", out)
	}
	return id
}
