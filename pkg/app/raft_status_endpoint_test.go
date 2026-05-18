package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// raftStatusResp mirrors the controller's JSON shape so the test can
// decode it without importing the internal controllers package.
type raftStatusResp struct {
	Enabled   bool `json:"enabled"`
	NumGroups int  `json:"numGroups"`
	Groups    []struct {
		ShardIdx       int    `json:"shardIdx"`
		IsLeader       bool   `json:"isLeader"`
		SelfID         string `json:"selfId"`
		SelfAddr       string `json:"selfAddr"`
		LeaderID       string `json:"leaderId"`
		LeaderAddr     string `json:"leaderAddr"`
		LeaderHTTPAddr string `json:"leaderHTTPAddr"`
		HasLeader      bool   `json:"hasLeader"`
	} `json:"groups"`
}

func TestRaftStatusEndpoint_RaftDisabled(t *testing.T) {
	pcfg, _ := json.Marshal(map[string]any{"path": t.TempDir() + "/pebble"})
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
	defer func() {
		if app.TracingShutdown != nil {
			_ = app.TracingShutdown(context.Background())
		}
	}()
	SetupMappings(app)
	srv := httptest.NewServer(app.Engine)
	defer srv.Close()

	resp := getStatus(t, srv.URL)
	if resp.Enabled {
		t.Errorf("raft disabled: want Enabled=false, got true")
	}
	if resp.NumGroups != 0 {
		t.Errorf("raft disabled: want NumGroups=0, got %d", resp.NumGroups)
	}
}

func TestRaftStatusEndpoint_SingleShard_Leader(t *testing.T) {
	d := openSingleNodeRaftApp(t)
	defer d.cleanup()

	// Wait for the lone node to elect.
	deadline := time.Now().Add(3 * time.Second)
	var resp raftStatusResp
	for time.Now().Before(deadline) {
		resp = getStatus(t, d.server.URL)
		if len(resp.Groups) == 1 && resp.Groups[0].IsLeader {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !resp.Enabled {
		t.Fatalf("want Enabled=true, got false: %+v", resp)
	}
	if resp.NumGroups != 1 {
		t.Errorf("want NumGroups=1, got %d", resp.NumGroups)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(resp.Groups))
	}
	g := resp.Groups[0]
	if !g.IsLeader {
		t.Errorf("single-node should be leader, got isLeader=false")
	}
	if g.SelfID != "node-1" {
		t.Errorf("SelfID: want node-1, got %q", g.SelfID)
	}
	if !g.HasLeader || g.LeaderID != "node-1" {
		t.Errorf("LeaderID: want node-1 + HasLeader=true, got %q + %v", g.LeaderID, g.HasLeader)
	}
	if g.LeaderHTTPAddr != "http://127.0.0.1:8080" {
		t.Errorf("LeaderHTTPAddr: want http://127.0.0.1:8080, got %q", g.LeaderHTTPAddr)
	}
}

type singleNodeRaftApp struct {
	app     *Application
	server  *httptest.Server
	cleanup func()
}

func openSingleNodeRaftApp(t *testing.T) singleNodeRaftApp {
	t.Helper()
	port := pickContiguousFreePorts(t, 1)
	pcfg, _ := json.Marshal(map[string]any{"path": t.TempDir() + "/pebble"})
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
		Raft: config.RaftConfig{
			Enabled:             true,
			SelfID:              "node-1",
			BindAddr:            "127.0.0.1:" + portString(port),
			Bootstrap:           true,
			PeerHTTPAddrs:       map[string]string{"node-1": "http://127.0.0.1:8080"},
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
	SetupMappings(app)
	srv := httptest.NewServer(app.Engine)
	return singleNodeRaftApp{
		app:    app,
		server: srv,
		cleanup: func() {
			srv.Close()
			if app.TracingShutdown != nil {
				_ = app.TracingShutdown(context.Background())
			}
		},
	}
}

func getStatus(t *testing.T, baseURL string) raftStatusResp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/codeq/raft/status", nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var out raftStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func portString(p int) string {
	// fmt.Sprintf("%d") with the strconv route avoids pulling in fmt
	// for one call when the rest of the file uses %s formatting.
	return intToString(p)
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
