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

// TestRaftBench_VsSingleNode measures full-cycle (create → claim →
// result) throughput against a 3-node raft cluster and a single-node
// Pebble baseline using the same REST harness. Apples-to-apples — the
// difference is purely the raft.Apply overhead per write.
//
// Skipped under -short. Run with:
//   go test -v -run TestRaftBench_VsSingleNode -count=1 -timeout=120s ./pkg/app
//
// Plan target (M1.T10): raft path ≥ 30k tasks/s. In practice on a
// loopback 3-node cluster the figure depends heavily on the host;
// the test prints both numbers and only fails if raft throughput
// drops below ~30% of the single-node baseline (the floor we expect
// from 1 raft RTT + 2 follower fsync on every write).
func TestRaftBench_VsSingleNode(t *testing.T) {
	if testing.Short() {
		t.Skip("bench is long; run without -short")
	}

	const (
		warmup     = 1 * time.Second
		window     = 5 * time.Second
		concurrent = 32
	)

	t.Run("single-node baseline", func(t *testing.T) {
		srv := startSingleNodeBenchServer(t)
		_, baselineOps := runCycleBench(t, srv, warmup, window, concurrent)
		t.Logf("single-node REST cycles/s: %.0f", baselineOps)
		// Save for the raft test below via a shared variable.
		baselineCycles.Store(int64(baselineOps))
	})

	t.Run("raft 3-node", func(t *testing.T) {
		baseline := float64(baselineCycles.Load())
		if baseline == 0 {
			t.Skip("baseline did not run")
		}

		ports := pickThreeFreePorts(t)
		peers := map[string]string{
			"node-1": "127.0.0.1:" + ports[0],
			"node-2": "127.0.0.1:" + ports[1],
			"node-3": "127.0.0.1:" + ports[2],
		}
		nodes := make([]*raftTestNode, 3)
		for i, id := range []string{"node-1", "node-2", "node-3"} {
			nodes[i] = startRaftNode(t, id, peers, i == 0)
		}
		t.Cleanup(func() {
			for _, n := range nodes {
				if n != nil && !n.closed.Load() {
					_ = n.shutdown()
				}
			}
		})
		leader, _ := waitForLeader(t, nodes, 5*time.Second)

		_, raftOps := runCycleBench(t, leader.server, warmup, window, concurrent)
		t.Logf("raft 3-node REST cycles/s: %.0f", raftOps)
		ratio := raftOps / baseline
		t.Logf("raft / baseline ratio: %.2f", ratio)

		// Floor at 30% of baseline. Below that something is wrong
		// (lock contention, transport retries, etc.) and the bench
		// should fail rather than report bogus throughput.
		if ratio < 0.30 {
			t.Fatalf("raft throughput %.0f cycles/s is %.0f%% of baseline %.0f — expected ≥30%%",
				raftOps, ratio*100, baseline)
		}
	})
}

var baselineCycles atomic.Int64

// startSingleNodeBenchServer brings up a non-raft Pebble Application
// — the baseline for the bench. Same REST surface, no replication.
func startSingleNodeBenchServer(t *testing.T) *httptest.Server {
	t.Helper()
	pcfg, _ := json.Marshal(map[string]any{"path": t.TempDir() + "/pebble"})
	cfg := benchBaseConfig(t, pcfg)
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

func benchBaseConfig(t *testing.T, pcfg []byte) *config.Config {
	t.Helper()
	return &config.Config{
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
	}
}

// runCycleBench fires N goroutines that each loop create → claim →
// submit-result against the given server. After a brief warmup it
// measures throughput over `window` and returns (totalOps, opsPerSec).
func runCycleBench(t *testing.T, server *httptest.Server, warmup, window time.Duration, concurrency int) (int64, float64) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}

	// Warmup: same loop, results discarded.
	wctx, wcancel := context.WithTimeout(context.Background(), warmup)
	runCycleLoop(wctx, client, server, concurrency, nil)
	wcancel()

	// Measurement.
	var ops atomic.Int64
	mctx, mcancel := context.WithTimeout(context.Background(), window)
	start := time.Now()
	runCycleLoop(mctx, client, server, concurrency, &ops)
	mcancel()
	elapsed := time.Since(start)

	return ops.Load(), float64(ops.Load()) / elapsed.Seconds()
}

// runCycleLoop spawns `concurrency` goroutines that each loop
// create → claim → submit-result until ctx is cancelled. ops, when
// non-nil, counts every completed cycle.
func runCycleLoop(ctx context.Context, client *http.Client, server *httptest.Server, concurrency int, ops *atomic.Int64) {
	var wg sync.WaitGroup
	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				taskID := benchCreate(ctx, client, server, id)
				if taskID == "" {
					return
				}
				claimed := benchClaim(ctx, client, server)
				if claimed == "" {
					continue
				}
				benchSubmit(ctx, client, server, claimed)
				if ops != nil {
					ops.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
}

func benchCreate(ctx context.Context, client *http.Client, server *httptest.Server, id int) string {
	body := fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"g":%d},"priority":5}`, id)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	taskID, _ := out["id"].(string)
	return taskID
}

func benchClaim(ctx context.Context, client *http.Client, server *httptest.Server) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/codeq/tasks/claim",
		strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	id, _ := out["id"].(string)
	return id
}

func benchSubmit(ctx context.Context, client *http.Client, server *httptest.Server, taskID string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/codeq/tasks/"+taskID+"/result",
		strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
}
