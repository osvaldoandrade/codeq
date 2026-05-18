package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestRaft_Mux_3Node_4Shard verifies the M2.T3 end-state: every Pebble
// shard's raft group shares a single TCP port per node. 3 nodes × 4
// shards = 12 raft groups across just 3 listeners (one per node).
// Same correctness checks as TestRaft_MultiShard_3Node — writes
// replicate, kill node, failover, all 60 tasks consistent on
// survivors — but with the mux transport.
func TestRaft_Mux_3Node_4Shard(t *testing.T) {
	const numShards = 4
	const numNodes = 3

	// Mux mode needs only one port per node — no contiguous range.
	// Pre-grab 3 free ports (any free ports, not contiguous).
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		"node-1": "127.0.0.1:" + ports[0],
		"node-2": "127.0.0.1:" + ports[1],
		"node-3": "127.0.0.1:" + ports[2],
	}

	nodes := make([]*raftTestNode, numNodes)
	for i, id := range []string{"node-2", "node-3"} {
		nodes[i+1] = startMuxRaftNode(t, id, peers, numShards, false)
	}
	nodes[0] = startMuxRaftNode(t, "node-1", peers, numShards, true)
	t.Cleanup(func() {
		for _, n := range nodes {
			if n != nil && !n.closed.Load() {
				_ = n.shutdown()
			}
		}
	})

	survivors := func() []*raftTestNode {
		out := make([]*raftTestNode, 0, len(nodes))
		for _, n := range nodes {
			if !n.closed.Load() {
				out = append(out, n)
			}
		}
		return out
	}

	ids := submitTasksAcrossNodes(t, survivors(), 40, 5*time.Second)
	for _, n := range survivors() {
		for _, id := range ids {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Fatalf("node %s missing task %s after initial writes", n.id, id)
			}
		}
	}

	t.Logf("killing %s", nodes[0].id)
	if err := nodes[0].shutdown(); err != nil {
		t.Fatalf("shutdown node-1: %v", err)
	}

	moreIDs := submitTasksAcrossNodes(t, survivors(), 20, 5*time.Second)
	ids = append(ids, moreIDs...)

	for _, n := range survivors() {
		for _, id := range ids {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Errorf("survivor %s missing task %s after failover", n.id, id)
			}
		}
	}
}

func startMuxRaftNode(t *testing.T, id string, peers map[string]string, numShards int, bootstrap bool) *raftTestNode {
	t.Helper()
	pebbleDir := t.TempDir() + "/pebble"
	pcfg, _ := json.Marshal(map[string]any{
		"path":      pebbleDir,
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
	server := httptest.NewServer(app.Engine)
	return &raftTestNode{
		id:       id,
		app:      app,
		server:   server,
		bindAddr: peers[id],
	}
}

