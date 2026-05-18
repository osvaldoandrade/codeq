package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestNewApplication_RaftEnabled boots codeq with raft.enabled=true,
// goes through the full create → claim → result REST cycle, and
// asserts the data lands on disk. Verifies the wireup in
// application_pebble.go correctly attaches the raft replicator to the
// pebble repository layer so writes flow through raft.Apply.
//
// Single-node bootstrap; multi-node failover lives in T9.
func TestNewApplication_RaftEnabled(t *testing.T) {
	dir := t.TempDir()
	pcfg, _ := json.Marshal(map[string]any{"path": dir + "/pebble"})

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
		RedisAddr:                          "127.0.0.1:0",
		Raft: config.RaftConfig{
			Enabled:             true,
			SelfID:              "node-1",
			BindAddr:            "127.0.0.1:0",
			Bootstrap:           true,
			HeartbeatMS:         50,
			ElectionMS:          50,
			LeaderLeaseMS:       50,
			CommitMS:            10,
			ApplyTimeoutSeconds: 2,
		},
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

	// Give raft a moment to elect (single-node bootstrap usually elects
	// within ~50ms with our tightened timeouts). Poll the API until
	// writes succeed.
	deadline := time.Now().Add(3 * time.Second)
	var lastErr string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks",
			strings.NewReader(`{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}`))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted {
			break
		}
		lastErr = string(body[:n])
		time.Sleep(50 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatalf("never elected leader / accepted writes: %s", lastErr)
	}

	// Now run the standard create → claim → result cycle. Each call
	// hits a write path that's routed through raft → FSM → local
	// pebble.
	claimReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/claim",
		strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
	claimReq.Header.Set("Authorization", "Bearer dev-token")
	claimReq.Header.Set("Content-Type", "application/json")
	claimResp, err := http.DefaultClient.Do(claimReq)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim expected 200, got %d", claimResp.StatusCode)
	}
	var claimed map[string]any
	_ = json.NewDecoder(claimResp.Body).Decode(&claimed)
	id, _ := claimed["id"].(string)
	if id == "" {
		t.Fatalf("claim returned no id: %+v", claimed)
	}

	resultReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/codeq/tasks/"+id+"/result",
		strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`))
	resultReq.Header.Set("Authorization", "Bearer dev-token")
	resultReq.Header.Set("Content-Type", "application/json")
	resultResp, err := http.DefaultClient.Do(resultReq)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	defer resultResp.Body.Close()
	if resultResp.StatusCode != http.StatusOK {
		t.Fatalf("result expected 200, got %d", resultResp.StatusCode)
	}

	// Sanity: fetch the result back. Read path is local, no raft hop.
	getReq, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/codeq/tasks/"+id+"/result", nil)
	getReq.Header.Set("Authorization", "Bearer dev-token")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get result: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get result expected 200, got %d", getResp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(getResp.Body).Decode(&out)
	rec, _ := out["result"].(map[string]any)
	if rec == nil || rec["status"] != "COMPLETED" {
		t.Errorf("unexpected result payload: %+v", out)
	}
}

// TestRaftConfig_MutualExclusion checks that the config validator
// rejects raft.enabled alongside cluster.enabled / sharding.enabled.
func TestRaftConfig_MutualExclusion(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Env:                     "dev",
			MaxAttemptsDefault:      5,
			BackoffPolicy:           "fixed",
			BackoffBaseSeconds:      1,
			BackoffMaxSeconds:       3,
			WorkerAudience:          "codeq-worker",
			AllowedClockSkewSeconds: 60,
			PersistenceProvider:     "pebble",
			Raft: config.RaftConfig{
				Enabled:  true,
				SelfID:   "node-1",
				BindAddr: ":7000",
			},
		}
	}

	c1 := base()
	c1.Cluster.Enabled = true
	if err := c1.Validate(); err == nil || !strings.Contains(err.Error(), "cluster.enabled") {
		t.Errorf("raft+cluster: want mutual-exclusion error, got %v", err)
	}

	c2 := base()
	c2.Sharding.Enabled = true
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "sharding.enabled") {
		t.Errorf("raft+sharding: want mutual-exclusion error, got %v", err)
	}

	c3 := base()
	c3.PersistenceProvider = "redis"
	if err := c3.Validate(); err == nil || !strings.Contains(err.Error(), "persistenceProvider=pebble") {
		t.Errorf("raft+redis: want persistence error, got %v", err)
	}

	c4 := base()
	c4.Raft.SelfID = ""
	if err := c4.Validate(); err == nil || !strings.Contains(err.Error(), "raft.selfId is required") {
		t.Errorf("raft no selfId: want error, got %v", err)
	}
}
