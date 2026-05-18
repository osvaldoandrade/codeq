package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestRaft_MultiShard_3Node failover is the M2 endgame proof:
// 3 codeq processes × 4 Pebble shards = 12 independent raft groups.
// Each shard's quorum is 2/3; failover survives any single-node loss.
// Writes hash to a shard via fnv64a (taskID), then to that shard's
// leader (whichever node currently holds it).
func TestRaft_MultiShard_3Node(t *testing.T) {
	const numShards = 4
	const numNodes = 3

	// Pre-grab numNodes × numShards contiguous ports, then split into
	// per-node blocks of numShards consecutive ports each.
	base := pickContiguousFreePorts(t, numNodes*numShards)
	nodeBases := make([]int, numNodes)
	for i := range nodeBases {
		nodeBases[i] = base + i*numShards
	}

	peers := map[string]string{
		"node-1": fmt.Sprintf("127.0.0.1:%d", nodeBases[0]),
		"node-2": fmt.Sprintf("127.0.0.1:%d", nodeBases[1]),
		"node-3": fmt.Sprintf("127.0.0.1:%d", nodeBases[2]),
	}

	// Start followers FIRST so their transports are listening before
	// node-1 bootstraps and starts dialing peers. Otherwise node-1's
	// initial election rounds hit connection-refused on every peer
	// and the cluster takes much longer to settle (or never does
	// within the test timeout).
	nodes := make([]*raftTestNode, numNodes)
	for i, id := range []string{"node-2", "node-3"} {
		nodes[i+1] = startMultiShardRaftNode(t, id, peers, numShards, false /* bootstrap */)
	}
	nodes[0] = startMultiShardRaftNode(t, "node-1", peers, numShards, true /* bootstrap */)
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

	// No explicit "wait for ready" — submitTasksAcrossNodes retries
	// on the next node on every ErrNotLeader, so the first few writes
	// naturally probe until the cluster has settled. Each call gets a
	// fresh UUID, so retries hash to a fresh shard.
	ids := submitTasksAcrossNodes(t, survivors(), 40, 5*time.Second)

	// Every node must answer GET /tasks/:id consistently.
	for _, n := range survivors() {
		for _, id := range ids {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Fatalf("node %s missing task %s after initial writes", n.id, id)
			}
		}
	}

	// Kill the first node. Each of its 4 raft groups had ~1/3 chance
	// of being leader; the other 2 nodes' groups for those shards will
	// trigger elections.
	t.Logf("killing %s", nodes[0].id)
	if err := nodes[0].shutdown(); err != nil {
		t.Fatalf("shutdown node-1: %v", err)
	}

	// Submit 20 more tasks against the 2 survivors. Retries on
	// ErrNotLeader drive re-election convergence — the test doesn't
	// need to know which shard re-elected on which survivor.
	moreIDs := submitTasksAcrossNodes(t, survivors(), 20, 5*time.Second)
	ids = append(ids, moreIDs...)

	// Final consistency check: every surviving node sees every task.
	for _, n := range survivors() {
		for _, id := range ids {
			body := getTask(t, n, id)
			if !strings.Contains(body, id) {
				t.Errorf("survivor %s missing task %s after failover", n.id, id)
			}
		}
	}
}

func startMultiShardRaftNode(t *testing.T, id string, peers map[string]string, numShards int, bootstrap bool) *raftTestNode {
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

// submitTasksAcrossNodes creates n tasks, rotating across the provided
// nodes on every attempt. On ErrNotLeader the next attempt picks the
// next node + generates a new UUID, so retries hash to fresh shards
// and converge on leaders within a few election timeouts.
func submitTasksAcrossNodes(t *testing.T, nodes []*raftTestNode, n int, timeout time.Duration) []string {
	t.Helper()
	if len(nodes) == 0 {
		t.Fatalf("submitTasksAcrossNodes: no nodes")
	}
	out := make([]string, 0, n)
	var counter atomic.Int64
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"i":%d},"priority":5}`, i)
		id, err := createOnAny(t, nodes, body, timeout, &counter)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		out = append(out, id)
	}
	return out
}

func createOnAny(t *testing.T, nodes []*raftTestNode, body string, timeout time.Duration, counter *atomic.Int64) (string, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody string
	for time.Now().Before(deadline) {
		node := nodes[int(counter.Add(1))%len(nodes)]
		req, _ := http.NewRequest(http.MethodPost, node.server.URL+"/v1/codeq/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusAccepted {
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			id, _ := out["id"].(string)
			return id, nil
		}
		b := make([]byte, 512)
		nn, _ := resp.Body.Read(b)
		resp.Body.Close()
		lastBody = string(b[:nn])
		if !strings.Contains(lastBody, "not leader") {
			return "", fmt.Errorf("status %d body=%s", resp.StatusCode, lastBody)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for leader across %d nodes (last=%s)", len(nodes), lastBody)
}
