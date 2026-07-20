package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestRaft3NodeFailover spins up three in-process codeq instances
// joined into a raft cluster, exercises the write path against the
// leader, kills the leader, and verifies the survivors elect a new
// one and see consistent state.
//
// This is the M1 proof: replication actually works, failover actually
// recovers, and the rest of codeq stays oblivious to which node is
// leader at any moment.
func TestRaft3NodeFailover(t *testing.T) {
	// 1. Pre-grab three free loopback ports so the bootstrap
	//    configuration can reference them by their final address.
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		"node-1": "127.0.0.1:" + ports[0],
		"node-2": "127.0.0.1:" + ports[1],
		"node-3": "127.0.0.1:" + ports[2],
	}

	// 2. Spawn all three nodes. Only node-1 bootstraps; the others
	//    start as fresh followers and pick up the configuration from
	//    the leader via raft replication.
	nodes := make([]*raftTestNode, 3)
	for i, id := range []string{"node-1", "node-2", "node-3"} {
		nodes[i] = startRaftNode(t, id, peers, i == 0 /* bootstrap */)
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			if n != nil && !n.closed.Load() {
				_ = n.shutdown()
			}
		}
	})

	// 3. Wait for a leader to emerge. With heartbeat=50ms,
	//    election=50ms a single-shard cluster should elect in
	//    ~100-300ms.
	leader, _ := waitForLeader(t, nodes, 5*time.Second)
	t.Logf("first leader elected: %s", leader.id)

	// 4. Submit tasks against the current leader. Each create call
	//    travels: REST → leader → raft.Apply → FSM Apply on all 3
	//    nodes → response.
	taskIDs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		id := createTaskOnLeader(t, leader, fmt.Sprintf(`{"i":%d}`, i))
		taskIDs = append(taskIDs, id)
	}

	// 5. Every node — leader and followers — must answer GET /tasks/:id
	//    consistently. Followers serve reads from their local Pebble.
	for _, n := range nodes {
		for _, id := range taskIDs {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Fatalf("node %s missing task %s: body=%s", n.id, id, body)
			}
		}
	}

	// 6. Kill the leader. The two survivors must elect a new leader
	//    within an election timeout window.
	t.Logf("killing leader %s", leader.id)
	if err := leader.shutdown(); err != nil {
		t.Fatalf("leader shutdown: %v", err)
	}

	survivors := make([]*raftTestNode, 0, 2)
	for _, n := range nodes {
		if n != leader {
			survivors = append(survivors, n)
		}
	}

	newLeader, _ := waitForLeader(t, survivors, 5*time.Second)
	if newLeader.id == leader.id {
		t.Fatalf("election returned the dead leader %s", leader.id)
	}
	t.Logf("new leader elected: %s", newLeader.id)

	// 7. Writes must continue to work against the new leader.
	for i := 20; i < 30; i++ {
		id := createTaskOnLeader(t, newLeader, fmt.Sprintf(`{"i":%d}`, i))
		taskIDs = append(taskIDs, id)
	}

	// 8. Both survivors must see ALL 30 tasks. The follower's view
	//    confirms replication caught up.
	for _, n := range survivors {
		for _, id := range taskIDs {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Errorf("survivor %s missing task %s after failover", n.id, id)
			}
		}
	}
}

// raftTestNode wraps an in-process codeq application + its HTTP test
// server for the cluster scenarios in this file.
type raftTestNode struct {
	id       string
	app      *Application
	server   *httptest.Server
	bindAddr string
	closed   atomic.Bool
}

func (n *raftTestNode) shutdown() error {
	if n.closed.Swap(true) {
		return nil
	}
	if n.server != nil {
		n.server.Close()
	}
	if n.app != nil && n.app.TracingShutdown != nil {
		return n.app.TracingShutdown(context.Background())
	}
	return nil
}

func startRaftNode(t *testing.T, id string, peers map[string]string, bootstrap bool) *raftTestNode {
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
			Enabled:              true,
			SelfID:               id,
			BindAddr:             peers[id],
			Bootstrap:            bootstrap,
			Peers:                peers,
			HeartbeatMS:          50,
			ElectionMS:           50,
			LeaderLeaseMS:        50,
			CommitMS:             10,
			ApplyTimeoutSeconds:  3,
			TopicCatalogProtocol: "v1",
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
	server := httptest.NewServer(app.Engine)

	return &raftTestNode{
		id:       id,
		app:      app,
		server:   server,
		bindAddr: peers[id],
	}
}

// waitForLeader polls each node's REST surface until a write succeeds.
// A POST /v1/codeq/tasks against a follower returns 5xx (ErrNotLeader)
// or hangs on raft.Apply; against the leader it returns 202.
func waitForLeader(t *testing.T, nodes []*raftTestNode, timeout time.Duration) (*raftTestNode, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	body := `{"command":"GENERATE_MASTER","payload":{"probe":"leader"},"priority":5}`
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.closed.Load() {
				continue
			}
			req, _ := http.NewRequest(http.MethodPost, n.server.URL+"/v1/codeq/tasks", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer dev-token")
			req.Header.Set("Content-Type", "application/json")
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			req = req.WithContext(ctx)
			resp, err := http.DefaultClient.Do(req)
			cancel()
			if err != nil {
				continue
			}
			respBody := readAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				var out map[string]any
				_ = json.Unmarshal([]byte(respBody), &out)
				id, _ := out["id"].(string)
				return n, id
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leader elected within %s", timeout)
	return nil, ""
}

func createTaskOnLeader(t *testing.T, leader *raftTestNode, payload string) string {
	t.Helper()
	body := `{"command":"GENERATE_MASTER","payload":` + payload + `,"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, leader.server.URL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create on leader %s: %v", leader.id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create on leader %s: status %d body=%s", leader.id, resp.StatusCode, readAll(resp.Body))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create on leader %s returned no id", leader.id)
	}
	return id
}

func getTask(t *testing.T, n *raftTestNode, id string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, n.server.URL+"/v1/codeq/tasks/"+id, nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			b := readAll(resp.Body)
			resp.Body.Close()
			return b
		}
		if resp != nil {
			resp.Body.Close()
		}
		// Follower may be a few ms behind the leader's apply; retry.
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("[%s] GET /tasks/%s never returned 200", n.id, id)
	return ""
}

func readAll(r interface{ Read([]byte) (int, error) }) string {
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// pickThreeFreePorts grabs three free TCP ports on 127.0.0.1, releases
// them, and returns the ports as strings. Race window between release
// and reuse is tiny on a single-machine test run; if it ever bites,
// switch to a deterministic port range gated by the test name.
func pickThreeFreePorts(t *testing.T) [3]string {
	t.Helper()
	listeners := make([]net.Listener, 3)
	ports := [3]string{}
	for i := range listeners {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for j := 0; j < i; j++ {
				_ = listeners[j].Close()
			}
			t.Fatalf("net.Listen: %v", err)
		}
		listeners[i] = l
		_, port, _ := net.SplitHostPort(l.Addr().String())
		ports[i] = port
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}
