package app

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

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/producerclient"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// TestRaftBench_GRPC measures full-cycle (create → claim → complete)
// throughput against a 3-node raft cluster using the producer + worker
// gRPC stream path instead of HTTP REST.
//
// Why: the HTTP smart-routing bench bottlenecked at ~3.9k cycles/s
// because every request went through http.Client connection pooling —
// mutex profile showed 28.74% of contention inside
// http.Transport.tryPutIdleConn. gRPC streams are persistent HTTP/2
// connections; concurrent calls on a session multiplex frames over
// the same connection with no per-request idle-pool churn.
//
// Skipped under -short. Run with:
//
//	go test -v -run='^TestRaftBench_GRPC$' -count=1 -timeout=180s ./pkg/app
func TestRaftBench_GRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("bench is long; run without -short")
	}

	const (
		warmup          = 2 * time.Second
		window          = 8 * time.Second
		concurrent      = 128
		numNodes        = 3
		sessionsPerNode = 4 // open multiple streams per node — each gRPC stream.Send serializes
		produceBatch    = 1 // 1 = single Produce. >1 enables ProduceBatch but tends to outrun worker drainage.
		workerBatch     = 0 // workerclient.Config.BatchSize — 0 = single-task (queue depth too shallow for >1)

	)

	measure := func(t *testing.T, numShards int) (createRate, completeRate float64) {
		t.Helper()
		nodes, cleanup := startGRPCCluster(t, numNodes, numShards)
		t.Cleanup(cleanup)

		// N producer sessions per node (gRPC stream.Send is serialized
		// per-stream) + one worker client per node.
		totalSessions := numNodes * sessionsPerNode
		prodSess := make([]*producerclient.Session, 0, totalSessions)
		workerClis := make([]*workerclient.Client, numNodes)
		cancels := make([]context.CancelFunc, 0, totalSessions+numNodes)
		t.Cleanup(func() {
			for _, c := range cancels {
				c()
			}
			for _, s := range prodSess {
				if s != nil {
					s.Close()
				}
			}
			for _, w := range workerClis {
				if w != nil {
					_ = w.Close()
				}
			}
		})
		for i, n := range nodes {
			for j := 0; j < sessionsPerNode; j++ {
				pcli, err := producerclient.New(producerclient.Config{
					Addr:  n.producerAddr,
					Token: "dev-token",
				})
				if err != nil {
					t.Fatalf("[%s sess-%d] producerclient.New: %v", n.id, j, err)
				}
				pctx, pcancel := context.WithCancel(context.Background())
				cancels = append(cancels, pcancel)
				sess, err := pcli.Connect(pctx)
				if err != nil {
					t.Fatalf("[%s sess-%d] Connect: %v", n.id, j, err)
				}
				prodSess = append(prodSess, sess)
			}

			wcli, err := workerclient.New(workerclient.Config{
				Addr:         n.workerAddr,
				Token:        "dev-token",
				Commands:     []string{"GENERATE_MASTER"},
				Concurrency:  128,
				BatchSize:    workerBatch,
				LeaseSeconds: 60,
			})
			if err != nil {
				t.Fatalf("[%s] workerclient.New: %v", n.id, err)
			}
			workerClis[i] = wcli
		}

		// Probe + warmup: let every shard elect a leader before we
		// start measuring. We don't need the IDs back; the gRPC path
		// itself surfaces ErrNotLeader inline.
		_ = grpcWarmup(prodSess, 4*time.Second)

		wctx, wcancel := context.WithTimeout(context.Background(), warmup)
		runGRPCCycle(wctx, prodSess, workerClis, concurrent, produceBatch, nil, nil)
		wcancel()

		var created, completed atomic.Int64
		mctx, mcancel := context.WithTimeout(context.Background(), window)
		start := time.Now()
		runGRPCCycle(mctx, prodSess, workerClis, concurrent, produceBatch, &created, &completed)
		mcancel()
		elapsed := time.Since(start)
		return float64(created.Load()) / elapsed.Seconds(),
			float64(completed.Load()) / elapsed.Seconds()
	}

	var single, multi struct{ create, complete float64 }
	t.Run("3-node × 1-shard (raft + gRPC)", func(t *testing.T) {
		single.create, single.complete = measure(t, 1)
		t.Logf("1-shard:  create=%.0f/s  complete=%.0f/s", single.create, single.complete)
	})
	t.Run("3-node × 4-shard (raft + gRPC)", func(t *testing.T) {
		multi.create, multi.complete = measure(t, 4)
		t.Logf("4-shard:  create=%.0f/s  complete=%.0f/s", multi.create, multi.complete)
	})

	if single.create == 0 || multi.create == 0 {
		t.Skip("one of the subtests didn't measure")
	}
	t.Logf("multi/single create ratio:   %.2fx", multi.create/single.create)
	t.Logf("multi/single complete ratio: %.2fx", multi.complete/single.complete)
}

// grpcNode is one in-process codeq app exposing an HTTP server (for
// the smart-routing 307 path that the raft layer needs to discover
// peers) plus gRPC producer + worker stream listeners.
type grpcNode struct {
	id           string
	server       *httptest.Server
	app          *Application
	producerAddr string
	workerAddr   string
}

// startGRPCCluster spins up a 3-node raft cluster where each node also
// runs producer + worker gRPC stream servers on dedicated ports. Mirrors
// the topology of startSmartCluster but adds the stream addrs to the
// Config so NewApplication starts the gRPC servers.
func startGRPCCluster(t *testing.T, numNodes, numShards int) ([]*grpcNode, func()) {
	t.Helper()
	raftPorts := pickThreeFreePorts(t)
	prodPorts := pickThreeFreePorts(t)
	workerPorts := pickThreeFreePorts(t)

	peers := map[string]string{
		"node-1": "127.0.0.1:" + raftPorts[0],
		"node-2": "127.0.0.1:" + raftPorts[1],
		"node-3": "127.0.0.1:" + raftPorts[2],
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

	nodes := make([]*grpcNode, numNodes)
	startOne := func(idx int, id string, bootstrap bool) {
		pcfg, _ := json.Marshal(map[string]any{
			"path":      t.TempDir() + "/pebble",
			"numShards": numShards,
		})
		producerAddr := "127.0.0.1:" + prodPorts[idx]
		workerAddr := "127.0.0.1:" + workerPorts[idx]
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
			ProducerStreamAddr:                 producerAddr,
			WorkerStreamAddr:                   workerAddr,
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
		a, err := NewApplication(cfg)
		if err != nil {
			t.Fatalf("[%s] NewApplication: %v", id, err)
		}
		SetupMappings(a)
		servers[idx].Config.Handler = a.Engine
		nodes[idx] = &grpcNode{
			id:           id,
			server:       servers[idx],
			app:          a,
			producerAddr: producerAddr,
			workerAddr:   workerAddr,
		}
	}

	// Followers first so their transports listen before node-1 bootstraps.
	startOne(1, "node-2", false)
	startOne(2, "node-3", false)
	startOne(0, "node-1", true)

	// Give the gRPC listeners a beat to accept dialer connections — the
	// producerclient.New / workerclient.New call below assumes the
	// server is ready.
	waitListening(t, []string{nodes[0].producerAddr, nodes[1].producerAddr, nodes[2].producerAddr})
	waitListening(t, []string{nodes[0].workerAddr, nodes[1].workerAddr, nodes[2].workerAddr})

	cleanup := func() {
		for _, n := range nodes {
			if n != nil && n.app != nil && n.app.TracingShutdown != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = n.app.TracingShutdown(ctx)
				cancel()
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

func waitListening(t *testing.T, addrs []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for _, addr := range addrs {
		for {
			conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("waitListening: %s never came up: %v", addr, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// grpcWarmup pumps a handful of creates across every session so each
// shard's raft group elects a leader before the measurement window.
// Errors are tolerated — most are "not leader" while elections settle.
func grpcWarmup(sessions []*producerclient.Session, timeout time.Duration) []string {
	body := []byte(`{"warmup":true}`)
	deadline := time.Now().Add(timeout)
	out := make([]string, 0, 32)
	for i := 0; i < 32 && time.Now().Before(deadline); i++ {
		sess := sessions[i%len(sessions)]
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		id, err := sess.Produce(ctx, producerclient.CreateRequest{
			Command: "GENERATE_MASTER",
			Payload: body,
		})
		cancel()
		if err == nil && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// runGRPCCycle drives sustained load through both stream paths. Producer
// goroutines pipeline ProduceBatch calls round-robin across all node
// sessions. Worker clients drain in parallel via Run with BatchSize per
// slot. Both batched paths amortise raft Apply cost across N tasks via
// the per-shard apply coalescer.
//
// `produceBatch` controls how many creates each producer goroutine
// sends per stream message; `workerBatch` is the worker's per-slot
// claim/complete batch (passed via workerclient.Config.BatchSize).
func runGRPCCycle(
	ctx context.Context,
	sessions []*producerclient.Session,
	workers []*workerclient.Client,
	concurrency int,
	produceBatch int,
	created, completed *atomic.Int64,
) {
	var wg sync.WaitGroup

	// Workers: start one Run per node-local client. Each client
	// internally fans out across its Concurrency slots.
	for _, w := range workers {
		wg.Add(1)
		go func(w *workerclient.Client) {
			defer wg.Done()
			_ = w.Run(ctx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
				if completed != nil {
					completed.Add(1)
				}
				return workerclient.Completed(nil)
			})
		}(w)
	}

	// Producers: N goroutines pumping creates round-robin across sessions.
	body := []byte(`{"bench":true}`)
	var counter atomic.Uint64
	if produceBatch <= 1 {
		for g := 0; g < concurrency; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					idx := int(counter.Add(1)) % len(sessions)
					sess := sessions[idx]
					id, err := sess.Produce(ctx, producerclient.CreateRequest{
						Command: "GENERATE_MASTER",
						Payload: body,
					})
					if err != nil {
						continue
					}
					if id != "" && created != nil {
						created.Add(1)
					}
				}
			}()
		}
	} else {
		// Build a static request template — N copies of the same
		// CreateRequest. The server picks UUIDs internally so each
		// element of the batch becomes a distinct task.
		reqs := make([]producerclient.CreateRequest, produceBatch)
		for i := range reqs {
			reqs[i] = producerclient.CreateRequest{
				Command: "GENERATE_MASTER",
				Payload: body,
			}
		}
		for g := 0; g < concurrency; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					idx := int(counter.Add(1)) % len(sessions)
					sess := sessions[idx]
					results, err := sess.ProduceBatch(ctx, reqs)
					if err != nil {
						continue
					}
					if created != nil {
						for _, r := range results {
							if r.Err == nil && r.TaskID != "" {
								created.Add(1)
							}
						}
					}
				}
			}()
		}
	}

	wg.Wait()
	_ = fmt.Sprintf // keep fmt import for future debug
}
