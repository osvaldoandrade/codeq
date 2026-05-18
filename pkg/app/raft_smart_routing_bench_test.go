package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestRaftBench_SmartRouting measures full-cycle (create → claim →
// submit) throughput against a 3-node raft cluster with the FULL
// smart-routing stack: mux transport (RAFT_MUX_ENABLED=true) +
// 307 redirect (RAFT_PEER_HTTP_ADDRS wired) + http.Client that
// follows redirects with the POST body preserved.
//
// Compares 1-shard vs 4-shard topology. With smart routing, each
// shard's commit pipeline runs independently and the client lands on
// the right leader in at most one redirect — the previous
// TestRaftBench_MultiShardScale couldn't show this because it
// rotated nodes manually on every retry.
//
// Skipped under -short. Run with:
//   go test -v -run TestRaftBench_SmartRouting -count=1 -timeout=120s ./pkg/app
func TestRaftBench_SmartRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("bench is long; run without -short")
	}

	const (
		warmup     = 1 * time.Second
		window     = 5 * time.Second
		concurrent = 32
		numNodes   = 3
	)

	measure := func(t *testing.T, numShards int) float64 {
		t.Helper()
		nodes, cleanup := startSmartCluster(t, numNodes, numShards)
		t.Cleanup(cleanup)

		// Warmup probe via smart-routing clients (follows 307).
		_ = smartSubmit(t, nodes, 8, 5*time.Second)

		var ops atomic.Int64
		wctx, wcancel := context.WithTimeout(context.Background(), warmup)
		runSmartCycle(wctx, nodes, concurrent, nil)
		wcancel()

		mctx, mcancel := context.WithTimeout(context.Background(), window)
		start := time.Now()
		runSmartCycle(mctx, nodes, concurrent, &ops)
		mcancel()
		elapsed := time.Since(start)
		return float64(ops.Load()) / elapsed.Seconds()
	}

	var single, multi float64
	t.Run("3-node × 1-shard (raft + smart routing)", func(t *testing.T) {
		single = measure(t, 1)
		t.Logf("1-shard cycles/s: %.0f", single)
	})
	t.Run("3-node × 4-shard (raft + smart routing)", func(t *testing.T) {
		multi = measure(t, 4)
		t.Logf("4-shard cycles/s: %.0f", multi)
	})

	if single == 0 || multi == 0 {
		t.Skip("one of the subtests didn't measure")
	}
	ratio := multi / single
	t.Logf("multi-shard / single-shard ratio: %.2fx", ratio)
}

// smartNode is one in-process codeq app fronted by an httptest.Server.
type smartNode struct {
	id     string
	server *httptest.Server
	app    *Application
}

// startSmartCluster pre-grabs HTTP URLs (via httptest.NewUnstartedServer),
// builds the per-node Application with full PeerHTTPAddrs wired, then
// swaps each server's handler from a placeholder to the real engine.
// This is the only way to construct PeerHTTPAddrs before NewApplication
// runs — codeq's HTTP URL only exists after httptest assigns the
// listener.
func startSmartCluster(t *testing.T, numNodes, numShards int) ([]*smartNode, func()) {
	t.Helper()
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		"node-1": "127.0.0.1:" + ports[0],
		"node-2": "127.0.0.1:" + ports[1],
		"node-3": "127.0.0.1:" + ports[2],
	}
	ids := []string{"node-1", "node-2", "node-3"}

	servers := make([]*httptest.Server, numNodes)
	httpAddrs := make(map[string]string, numNodes)
	for i, id := range ids {
		s := httptest.NewUnstartedServer(http.NotFoundHandler())
		s.Start()
		servers[i] = s
		httpAddrs[id] = s.URL
	}

	nodes := make([]*smartNode, numNodes)
	startOne := func(idx int, id string, bootstrap bool) {
		pcfg, _ := json.Marshal(map[string]any{
			"path":      t.TempDir() + "/pebble",
			"numShards": numShards,
		})
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
				SelfID:              id,
				BindAddr:            peers[id],
				Bootstrap:           bootstrap,
				Peers:               peers,
				PeerHTTPAddrs:       httpAddrs,
				MuxEnabled:          true,
				HeartbeatMS:         50,
				ElectionMS:          50,
				LeaderLeaseMS:       50,
				CommitMS:            10,
				ApplyTimeoutSeconds: 3,
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("[%s] validate: %v", id, err)
		}
		app, err := NewApplication(cfg)
		if err != nil {
			t.Fatalf("[%s] NewApplication: %v", id, err)
		}
		SetupMappings(app)
		servers[idx].Config.Handler = app.Engine
		nodes[idx] = &smartNode{id: id, server: servers[idx], app: app}
	}

	// Followers first so their transports listen before node-1
	// bootstraps.
	startOne(1, "node-2", false)
	startOne(2, "node-3", false)
	startOne(0, "node-1", true)

	cleanup := func() {
		for _, n := range nodes {
			if n != nil && n.app != nil && n.app.TracingShutdown != nil {
				_ = n.app.TracingShutdown(context.Background())
			}
		}
		for _, s := range servers {
			if s != nil {
				s.Close()
			}
		}
	}
	return nodes, cleanup
}

// smartSubmit submits n probe tasks across the cluster to confirm
// every shard's leader is elected before the measurement window.
// Uses the smart-routing client (follows 307).
func smartSubmit(t *testing.T, nodes []*smartNode, n int, timeout time.Duration) []string {
	t.Helper()
	client := newSmartClient(3 * time.Second)
	var counter atomic.Int64
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		node := nodes[int(counter.Add(1))%len(nodes)]
		body := fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"probe":%d},"priority":5}`, i)
		id := smartPost(t, client, node.server.URL+"/v1/codeq/tasks", body, timeout)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// runSmartCycle is the throughput driver. Each goroutine picks any
// node, runs create → claim → submit. Smart-routing client follows
// the 307 to the leader transparently — no manual node rotation.
func runSmartCycle(ctx context.Context, nodes []*smartNode, concurrency int, ops *atomic.Int64) {
	var wg sync.WaitGroup
	var counter atomic.Int64
	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := newSmartClient(3 * time.Second)
			for {
				if ctx.Err() != nil {
					return
				}
				node := nodes[int(counter.Add(1))%len(nodes)]
				taskID := smartPost(nil, client, node.server.URL+"/v1/codeq/tasks",
					fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"g":%d},"priority":5}`, id), 0)
				if taskID == "" {
					continue
				}
				node = nodes[int(counter.Add(1))%len(nodes)]
				claimedRaw := smartPostRaw(client, node.server.URL+"/v1/codeq/tasks/claim",
					`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`)
				if claimedRaw == "" {
					continue
				}
				var out map[string]any
				_ = json.Unmarshal([]byte(claimedRaw), &out)
				cid, _ := out["id"].(string)
				if cid == "" {
					continue
				}
				node = nodes[int(counter.Add(1))%len(nodes)]
				_ = smartPostRaw(client, node.server.URL+"/v1/codeq/tasks/"+cid+"/result",
					`{"status":"COMPLETED","result":{"ok":true}}`)
				if ops != nil {
					ops.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
}

// newSmartClient returns an http.Client that follows 307 redirects
// (default behavior — POST + body preserved per RFC 7231). Timeouts
// kept high so a long redirect chain across a slow election doesn't
// poison the measurement.
func newSmartClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func smartPost(t *testing.T, client *http.Client, url, body string, timeout time.Duration) string {
	deadline := time.Now()
	if timeout > 0 {
		deadline = deadline.Add(timeout)
	}
	for {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if timeout == 0 || time.Now().After(deadline) {
				return ""
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusAccepted {
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			id, _ := out["id"].(string)
			return id
		}
		resp.Body.Close()
		if timeout == 0 {
			return ""
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func smartPostRaw(client *http.Client, url, body string) string {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return ""
	}
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}
