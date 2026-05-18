package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestWebhookCoverage_E2E exercises the three webhook paths added by
// PR #576 end-to-end against a live Pebble-backed codeq:
//   - BatchSubmit fan-out
//   - Nack → DLQ when attempts exhaust
//   - Reaper lease-expired → DLQ
//
// Each path drives the REST surface and asserts the per-task webhook
// callback actually fires with the expected payload. A captureSink
// records POSTs keyed by taskId so the asserts don't race on delivery
// order.
func TestWebhookCoverage_E2E(t *testing.T) {
	sink := newCaptureSink()
	hookSrv := httptest.NewServer(sink)
	defer hookSrv.Close()

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
		MaxAttemptsDefault:                 1, // first Nack lands the task in DLQ
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
	defer func() {
		if app.TracingShutdown != nil {
			_ = app.TracingShutdown(context.Background())
		}
	}()
	SetupMappings(app)

	server := httptest.NewServer(app.Engine)
	defer server.Close()

	t.Run("BatchSubmit fans webhook for every successful result", func(t *testing.T) {
		t1 := createTaskWithHook(t, server.URL, hookSrv.URL+"/batch/a")
		t2 := createTaskWithHook(t, server.URL, hookSrv.URL+"/batch/b")
		claimTaskByID(t, server.URL, t1)
		claimTaskByID(t, server.URL, t2)

		body := `{"results":[
			{"taskId":"` + t1 + `","status":"COMPLETED","result":{"ok":true}},
			{"taskId":"` + t2 + `","status":"FAILED","error":"intentional"}
		]}`
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/batch/results", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /tasks/batch/results: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 from batch result, got %d", resp.StatusCode)
		}
		resp.Body.Close()

		c1 := sink.wait(t, t1, 3*time.Second)
		c2 := sink.wait(t, t2, 3*time.Second)
		if c1["status"] != "COMPLETED" {
			t.Errorf("t1 status: want COMPLETED, got %v", c1["status"])
		}
		if c2["status"] != "FAILED" || c2["error"] != "intentional" {
			t.Errorf("t2: want FAILED/intentional, got status=%v error=%v", c2["status"], c2["error"])
		}
	})

	t.Run("Nack max-attempts fires webhook with FAILED+reason", func(t *testing.T) {
		id := createTaskWithHook(t, server.URL, hookSrv.URL+"/nack/x")
		claimTaskByID(t, server.URL, id)

		nackBody := `{"reason":"fatal-test","delaySeconds":0}`
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/"+id+"/nack", strings.NewReader(nackBody))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /nack: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("nack: expected 200, got %d", resp.StatusCode)
		}
		resp.Body.Close()

		c := sink.wait(t, id, 3*time.Second)
		if c["status"] != "FAILED" {
			t.Errorf("status: want FAILED, got %v", c["status"])
		}
		if c["error"] != "fatal-test" {
			t.Errorf("error: want fatal-test, got %v", c["error"])
		}
	})

	t.Run("Reaper LEASE_EXPIRED → DLQ fires webhook", func(t *testing.T) {
		id := createTaskWithHook(t, server.URL, hookSrv.URL+"/reaper/y")
		// Claim with a 1-second lease; the reaper sweep (2s default
		// interval) will requeue once the lease elapses. MaxAttempts=1
		// from the config means the requeue lands directly in DLQ.
		claimReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/claim", strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":1,"waitSeconds":0}`))
		claimReq.Header.Set("Authorization", "Bearer dev-token")
		claimReq.Header.Set("Content-Type", "application/json")
		claimResp, err := http.DefaultClient.Do(claimReq)
		if err != nil {
			t.Fatalf("POST /claim: %v", err)
		}
		if claimResp.StatusCode != http.StatusOK {
			t.Fatalf("claim: expected 200, got %d", claimResp.StatusCode)
		}
		claimResp.Body.Close()
		// Don't submit; let the lease expire and the reaper take over.
		c := sink.wait(t, id, 8*time.Second)
		if c["status"] != "FAILED" {
			t.Errorf("status: want FAILED, got %v", c["status"])
		}
		if c["error"] != "LEASE_EXPIRED" {
			t.Errorf("error: want LEASE_EXPIRED, got %v", c["error"])
		}
	})
}

// captureSink is an http.Handler that buffers every webhook POST keyed
// by taskId. Tests pull entries with wait() to dodge delivery races.
type captureSink struct {
	mu sync.Mutex
	// per-taskID single-shot channel; created lazily on first wait.
	pending map[string]chan map[string]any
	// stash payloads that arrived before wait() registered the channel.
	stash map[string][]map[string]any
}

func newCaptureSink() *captureSink {
	return &captureSink{
		pending: make(map[string]chan map[string]any),
		stash:   make(map[string][]map[string]any),
	}
}

func (s *captureSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	var payload map[string]any
	_ = json.Unmarshal(b, &payload)
	id, _ := payload["taskId"].(string)
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if ch, ok := s.pending[id]; ok {
		// Non-blocking send; if a previous payload was already queued,
		// drop the duplicate so the test sees the first one.
		select {
		case ch <- payload:
		default:
		}
	} else {
		s.stash[id] = append(s.stash[id], payload)
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// wait blocks until a webhook for the given task lands or timeout fires.
func (s *captureSink) wait(t *testing.T, taskID string, timeout time.Duration) map[string]any {
	t.Helper()
	s.mu.Lock()
	if buf := s.stash[taskID]; len(buf) > 0 {
		payload := buf[0]
		s.stash[taskID] = buf[1:]
		s.mu.Unlock()
		return payload
	}
	ch, ok := s.pending[taskID]
	if !ok {
		ch = make(chan map[string]any, 1)
		s.pending[taskID] = ch
	}
	s.mu.Unlock()

	select {
	case payload := <-ch:
		return payload
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for webhook for task %s", taskID)
		return nil
	}
}

func createTaskWithHook(t *testing.T, base, hook string) string {
	t.Helper()
	body := `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5,"webhook":"` + hook + `"}`
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

func claimTaskByID(t *testing.T, base, wantID string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/codeq/tasks/claim", strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if got, _ := out["id"].(string); got != wantID {
		t.Fatalf("claim returned wrong task: got %s, want %s", got, wantID)
	}
}
