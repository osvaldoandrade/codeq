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

// TestRaft_307RedirectsToLeader verifies that hitting a follower with
// a write returns HTTP 307 + Location pointing at the leader's HTTP
// URL. Go's http.Client follows the redirect transparently.
//
// Single-shard 3-node setup so the test can pre-grab a known leader
// and a known follower without race.
func TestRaft_307RedirectsToLeader(t *testing.T) {
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		"node-1": "127.0.0.1:" + ports[0],
		"node-2": "127.0.0.1:" + ports[1],
		"node-3": "127.0.0.1:" + ports[2],
	}

	// Start the apps. node-1 bootstraps; node-2 and node-3 follow.
	// We need their HTTP URLs to fill PeerHTTPAddrs — created BEFORE
	// the apps so each app's config can reference the others.
	srvs := make([]*httptest.Server, 3)
	apps := make([]*Application, 3)
	httpAddrs := make(map[string]string)

	// Pre-listen on the HTTP ports via httptest's port-picking, then
	// reuse: build httptest.NewUnstartedServer, learn its URL, build
	// PeerHTTPAddrs, then start. The trick — httptest.NewUnstartedServer
	// needs a Handler at construction but we don't have one yet
	// (Application engine is built inside startup). So: start with a
	// placeholder, learn the URL, then swap.
	ids := []string{"node-1", "node-2", "node-3"}
	for i, id := range ids {
		s := httptest.NewUnstartedServer(http.NotFoundHandler())
		s.Start()
		httpAddrs[id] = s.URL
		srvs[i] = s
	}
	t.Cleanup(func() {
		for _, s := range srvs {
			if s != nil {
				s.Close()
			}
		}
		for _, a := range apps {
			if a != nil && a.TracingShutdown != nil {
				_ = a.TracingShutdown(context.Background())
			}
		}
	})

	// Start the apps. Followers first to be listening before
	// bootstrapper dials.
	for i, id := range []string{"node-2", "node-3", "node-1"} {
		// Map id back to index in ids/peers/srvs.
		var origIdx int
		for k, v := range ids {
			if v == id {
				origIdx = k
				break
			}
		}
		bootstrap := id == "node-1"
		_ = i
		app, err := newRaftAppFor307(t, id, peers, httpAddrs, bootstrap)
		if err != nil {
			t.Fatalf("[%s] NewApplication: %v", id, err)
		}
		SetupMappings(app)
		// Swap the httptest server's handler from NotFound to the app
		// engine. The httptest server keeps its listener and URL.
		srvs[origIdx].Config.Handler = app.Engine
		apps[origIdx] = app
	}

	// Wait until any node accepts a write — at that point its raft
	// group has elected.
	deadline := time.Now().Add(5 * time.Second)
	leader := -1
	for time.Now().Before(deadline) {
		for i := range ids {
			if probeWrite(srvs[i].URL) == http.StatusAccepted {
				leader = i
				break
			}
		}
		if leader >= 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if leader < 0 {
		t.Fatal("no leader emerged within 5s")
	}
	t.Logf("leader is %s", ids[leader])

	// Pick a follower deterministically.
	follower := (leader + 1) % len(ids)
	t.Logf("follower: %s", ids[follower])

	// Send a write to the follower WITHOUT following redirects, so we
	// can inspect the 307 response directly.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 3 * time.Second,
	}
	body := `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, srvs[follower].URL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST to follower: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 from follower, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("307 response missing Location header")
	}
	if !strings.HasPrefix(location, srvs[leader].URL) {
		t.Errorf("Location %q should start with leader URL %q", location, srvs[leader].URL)
	}
	if !strings.HasSuffix(location, "/v1/codeq/tasks") {
		t.Errorf("Location %q should end with the request path", location)
	}

	// Verify a follow-the-redirect client (standard Go default)
	// succeeds with a single high-level call.
	followClient := &http.Client{Timeout: 3 * time.Second}
	req2, _ := http.NewRequest(http.MethodPost, srvs[follower].URL+"/v1/codeq/tasks", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer dev-token")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := followClient.Do(req2)
	if err != nil {
		t.Fatalf("POST with redirect follow: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Errorf("after redirect: want 202, got %d", resp2.StatusCode)
	}
}

func probeWrite(serverURL string) int {
	body := `{"command":"GENERATE_MASTER","payload":{"probe":true},"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, serverURL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func newRaftAppFor307(t *testing.T, id string, peers, httpAddrs map[string]string, bootstrap bool) (*Application, error) {
	t.Helper()
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
			SelfID:              id,
			BindAddr:            peers[id],
			Bootstrap:           bootstrap,
			Peers:               peers,
			PeerHTTPAddrs:       httpAddrs,
			HeartbeatMS:         50,
			ElectionMS:          50,
			LeaderLeaseMS:       50,
			CommitMS:            10,
			ApplyTimeoutSeconds: 3,
		},
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return NewApplication(cfg)
}
