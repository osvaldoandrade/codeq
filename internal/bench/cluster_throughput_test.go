package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/producerclient"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// Phase 4 throughput tests: bring up N codeq nodes in-process with the
// cluster ring enabled, exercise them via the Phase 2 worker stream and
// Phase 3 producer stream simultaneously, and report aggregate
// tasks-completed-per-second.
//
//	go test -run='^TestClusterThroughput' -count=1 -timeout=240s ./internal/bench/...
//
// Each node:
//   - has its own Pebble directory
//   - listens on a dedicated cluster gRPC port (routing peer)
//   - listens on dedicated producer + worker stream ports
//   - exposes the standard REST surface for orchestration
//
// The test scales producer/worker pools across all nodes and measures
// how aggregate throughput moves vs the single-node baseline. Skip with
// -short.

const phase4RunDuration = 6 * time.Second

type clusterNode struct {
	id            string
	httpURL       string
	producerAddr  string
	workerAddr    string
	pebblePath    string
	tracingClose  func(context.Context) error
	httpSrv       *httptest.Server
}

func freeAddr(tb testing.TB) (string, int) {
	tb.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().(*net.TCPAddr)
	_ = lis.Close()
	return fmt.Sprintf("127.0.0.1:%d", addr.Port), addr.Port
}

// bootClusterFleet boots `n` codeq nodes wired into one ring. Each node
// gets its own cluster gRPC + producer stream + worker stream listener
// plus a Pebble directory. Returns the slice in ring order.
func bootClusterFleet(t *testing.T, n int) []*clusterNode {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)

	specs := make([]config.ClusterNodeSpec, 0, n)
	nodes := make([]*clusterNode, 0, n)
	for i := range n {
		addr, _ := freeAddr(t)
		specs = append(specs, config.ClusterNodeSpec{
			ID:       fmt.Sprintf("node-%d", i),
			GRPCAddr: addr,
		})
	}

	producerCfg, _ := json.Marshal(map[string]any{
		"token":   phase2ProducerToken,
		"subject": "producer-bench",
		"raw":     map[string]any{"role": "ADMIN", "tenantId": phase2Tenant},
	})
	workerCfg, _ := json.Marshal(map[string]any{
		"token":      phase2WorkerToken,
		"subject":    "worker-bench",
		"scopes":     []string{"codeq:claim", "codeq:heartbeat", "codeq:abandon", "codeq:nack", "codeq:result", "codeq:subscribe"},
		"eventTypes": []string{"*"},
		"raw":        map[string]any{"tenantId": phase2Tenant},
	})

	for i, spec := range specs {
		pebbleDir := t.TempDir()
		pebbleCfg, _ := json.Marshal(map[string]any{"path": pebbleDir, "fsyncOnCommit": false})
		producerAddr, _ := freeAddr(t)
		workerAddr, _ := freeAddr(t)

		cfg := &config.Config{
			Env:                                "dev",
			Timezone:                           "UTC",
			LogLevel:                           "error",
			LogFormat:                          "json",
			DefaultLeaseSeconds:                300,
			RequeueInspectLimit:                50,
			LocalArtifactsDir:                  t.TempDir(),
			MaxAttemptsDefault:                 5,
			BackoffPolicy:                      "fixed",
			BackoffBaseSeconds:                 1,
			BackoffMaxSeconds:                  3,
			WebhookHmacSecret:                  "bench-secret",
			WorkerAudience:                     "codeq-worker",
			SubscriptionMinIntervalSeconds:     5,
			SubscriptionCleanupIntervalSeconds: 60,
			ResultWebhookMaxAttempts:           3,
			ResultWebhookBaseBackoffSeconds:    1,
			ResultWebhookMaxBackoffSeconds:     2,
			ProducerAuthProvider:               "static",
			ProducerAuthConfig:                 producerCfg,
			WorkerAuthProvider:                 "static",
			WorkerAuthConfig:                   workerCfg,
			PersistenceProvider:                "pebble",
			PersistenceConfig:                  pebbleCfg,
			RedisAddr:                          "127.0.0.1:0",
			ProducerStreamAddr:                 producerAddr,
			WorkerStreamAddr:                   workerAddr,
			RateLimit:                          config.RateLimitConfig{},
			Cluster: config.ClusterConfig{
				Enabled:  true,
				SelfID:   spec.ID,
				GRPCAddr: spec.GRPCAddr,
				Nodes:    specs,
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate %s: %v", spec.ID, err)
		}
		a, err := app.NewApplication(cfg)
		if err != nil {
			t.Fatalf("NewApplication %s: %v", spec.ID, err)
		}
		app.SetupMappings(a)
		httpSrv := httptest.NewServer(a.Engine)
		nd := &clusterNode{
			id:           spec.ID,
			httpURL:      httpSrv.URL,
			producerAddr: producerAddr,
			workerAddr:   workerAddr,
			pebblePath:   pebbleDir,
			tracingClose: a.TracingShutdown,
			httpSrv:      httpSrv,
		}
		nodes = append(nodes, nd)
		_ = i
	}
	t.Cleanup(func() {
		// Stop HTTP first (drains in-flight REST), then ask the App to
		// tear down its gRPC + Pebble + reaper goroutines.
		for _, nd := range nodes {
			nd.httpSrv.Close()
		}
		for _, nd := range nodes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = nd.tracingClose(ctx)
			cancel()
		}
	})
	// Wait for every gRPC listener (cluster + producer + worker) to bind.
	time.Sleep(300 * time.Millisecond)
	return nodes
}

// runStreamBench fires P producer sessions × C producer concurrency each,
// W worker sessions × C worker concurrency each, across the fleet, for
// the configured window. Producers and workers are round-robined across
// nodes so traffic enters every node.
func runStreamBench(t *testing.T, nodes []*clusterNode, producerSessions, producerConc, workerSessions, workerConc int) (createdN, completedN int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), phase4RunDuration+30*time.Second)
	defer cancel()
	deadline := time.Now().Add(phase4RunDuration)

	body := []byte(`{"bench":true}`)
	var created atomic.Int64
	var completed atomic.Int64

	var wg sync.WaitGroup

	// Producers
	for p := range producerSessions {
		node := nodes[p%len(nodes)]
		wg.Go(func() {
			cli, err := producerclient.New(producerclient.Config{
				Addr:  node.producerAddr,
				Token: phase2ProducerToken,
			})
			if err != nil {
				t.Errorf("producer New: %v", err)
				return
			}
			defer cli.Close()
			sess, err := cli.Connect(ctx)
			if err != nil {
				t.Errorf("producer Connect: %v", err)
				return
			}
			defer sess.Close()

			var inner sync.WaitGroup
			for range producerConc {
				inner.Go(func() {
					for time.Now().Before(deadline) {
						_, err := sess.Produce(ctx, producerclient.CreateRequest{
							Command: "GENERATE_MASTER",
							Payload: body,
						})
						if err != nil {
							return
						}
						created.Add(1)
					}
				})
			}
			inner.Wait()
		})
	}

	// Workers
	for w := range workerSessions {
		node := nodes[w%len(nodes)]
		wg.Go(func() {
			cli, err := workerclient.New(workerclient.Config{
				Addr:         node.workerAddr,
				Token:        phase2WorkerToken,
				Commands:     []string{"GENERATE_MASTER"},
				Concurrency:  workerConc,
				LeaseSeconds: 300,
			})
			if err != nil {
				t.Errorf("worker New: %v", err)
				return
			}
			defer cli.Close()
			runCtx, runCancel := context.WithDeadline(ctx, deadline)
			defer runCancel()
			_ = cli.Run(runCtx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
				completed.Add(1)
				return workerclient.Completed(nil)
			})
		})
	}

	wg.Wait()
	return created.Load(), completed.Load()
}

func TestClusterThroughput_StairStep(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster bench is long; run without -short")
	}

	sizes := []int{1, 2, 4}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("nodes=%d", n), func(t *testing.T) {
			nodes := bootClusterFleet(t, n)

			// Keep per-node load constant so we can read scaling: each node
			// gets 1 producer session × 16 concurrency and 1 worker session
			// × 32 concurrency.
			created, completed := runStreamBench(t, nodes, n, 16, n, 32)
			elapsed := phase4RunDuration
			createRate := float64(created) / elapsed.Seconds()
			completeRate := float64(completed) / elapsed.Seconds()
			t.Logf("n=%d  created=%-7d (%6.0f/s)  completed=%-7d (%6.0f/s)",
				n, created, createRate, completed, completeRate)
		})
	}
}

// TestClusterThroughput_VsSingleNode is the same workload at fixed
// load, comparing 1 node vs 4 nodes head-to-head so we can read the
// scaling factor directly. Useful when investigating cross-node
// overhead under stable load.
func TestClusterThroughput_VsSingleNode(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster bench is long; run without -short")
	}
	const totalProducerConc = 64
	const totalWorkerConc = 128

	for _, n := range []int{1, 4} {
		t.Run(fmt.Sprintf("nodes=%d", n), func(t *testing.T) {
			nodes := bootClusterFleet(t, n)
			// Spread the same total concurrency across nodes.
			perSessionProdConc := totalProducerConc / n
			perSessionWorkConc := totalWorkerConc / n
			created, completed := runStreamBench(t, nodes, n, perSessionProdConc, n, perSessionWorkConc)
			elapsed := phase4RunDuration
			t.Logf("n=%d  created=%-7d (%6.0f/s)  completed=%-7d (%6.0f/s)",
				n, created, float64(created)/elapsed.Seconds(), completed, float64(completed)/elapsed.Seconds())
		})
	}
}

// Tiny convenience: make the HTTP server pointer addressable. The
// httptest.Server type fields are public but we don't currently need
// them here; this keeps the import alive if the test file is trimmed.
var _ = (&http.Client{}).Do
