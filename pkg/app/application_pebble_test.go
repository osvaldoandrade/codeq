package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestNewApplication_Pebble exercises the Pebble dispatch path end-to-end:
// the full app boots, mounts the HTTP engine, and creates+claims+results a
// task through the public REST surface. This is the smoke test that proves
// the persistence switch actually runs.
func TestNewApplication_Pebble(t *testing.T) {
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
		ResultWebhookMaxAttempts:           3,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
		ProducerAuthProvider:               "static",
		ProducerAuthConfig:                 json.RawMessage(`{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN","tenantId":"dev-tenant"}}`),
		WorkerAuthProvider:                 "static",
		WorkerAuthConfig:                   json.RawMessage(`{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"],"raw":{"tenantId":"dev-tenant"}}`),
		PersistenceProvider:                "pebble",
		PersistenceConfig:                  pcfg,
		// RedisAddr stays unset; the Pebble path still constructs a
		// redis client for ratelimit but we keep the call no-op-friendly
		// by leaving the in-memory path enabled (ratelimit treats zero
		// rate as disabled).
		RedisAddr: "127.0.0.1:0",
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

	// Enqueue a task.
	body := `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /tasks: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Claim
	claimReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/claim", strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
	claimReq.Header.Set("Authorization", "Bearer dev-token")
	claimReq.Header.Set("Content-Type", "application/json")
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("POST /claim: %v", err)
	}
	if claimResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from claim, got %d", claimResp.StatusCode)
	}
	var claimed map[string]any
	_ = json.NewDecoder(claimResp.Body).Decode(&claimed)
	claimResp.Body.Close()
	id, _ := claimed["id"].(string)
	if id == "" {
		t.Fatalf("claim returned no id: %+v", claimed)
	}

	// Submit result
	resultReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/"+id+"/result", strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`))
	resultReq.Header.Set("Authorization", "Bearer dev-token")
	resultReq.Header.Set("Content-Type", "application/json")
	resultResp, err := http.DefaultClient.Do(resultReq)
	if err != nil {
		t.Fatalf("POST /result: %v", err)
	}
	if resultResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from result, got %d", resultResp.StatusCode)
	}
	resultResp.Body.Close()
}
