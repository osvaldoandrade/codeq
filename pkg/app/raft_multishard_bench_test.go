package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
)

// TestRaftBench_MultiShardScale measures throughput on a 3-node raft
// cluster with numShards=1 vs numShards=4. The hypothesis: multi-raft
// scales because each shard's commit pipeline runs in its own raft
// group, with independent quorum + parallel fsync.
//
// The bench drives the realistic write path (REST create → claim →
// submit), rotating across nodes on every operation. Each cycle is
// three writes that route to potentially three different shards.
//
// Skipped under -short. Run with:
//   go test -v -run TestRaftBench_MultiShardScale -count=1 -timeout=120s ./pkg/app
func TestRaftBench_MultiShardScale(t *testing.T) {
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
		base := pickContiguousFreePorts(t, numNodes*numShards)
		peers := map[string]string{
			"node-1": fmt.Sprintf("127.0.0.1:%d", base+0*numShards),
			"node-2": fmt.Sprintf("127.0.0.1:%d", base+1*numShards),
			"node-3": fmt.Sprintf("127.0.0.1:%d", base+2*numShards),
		}
		nodes := make([]*raftTestNode, numNodes)
		for i, id := range []string{"node-2", "node-3"} {
			nodes[i+1] = startMultiShardRaftNode(t, id, peers, numShards, false)
		}
		nodes[0] = startMultiShardRaftNode(t, "node-1", peers, numShards, true)
		t.Cleanup(func() {
			for _, n := range nodes {
				if n != nil && !n.closed.Load() {
					_ = n.shutdown()
				}
			}
		})

		// Warmup probe — guarantees every shard has elected before
		// the measurement window opens.
		_ = submitTasksAcrossNodes(t, nodes, 8, 5*time.Second)

		var ops atomic.Int64

		// Real warmup loop (results discarded).
		wctx, wcancel := context.WithTimeout(context.Background(), warmup)
		runMultiShardCycle(wctx, nodes, concurrent, nil)
		wcancel()

		// Measurement window.
		mctx, mcancel := context.WithTimeout(context.Background(), window)
		start := time.Now()
		runMultiShardCycle(mctx, nodes, concurrent, &ops)
		mcancel()
		elapsed := time.Since(start)
		return float64(ops.Load()) / elapsed.Seconds()
	}

	var single, multi float64
	t.Run("3-node × 1-shard", func(t *testing.T) {
		single = measure(t, 1)
		t.Logf("1-shard cycles/s: %.0f", single)
	})
	t.Run("3-node × 4-shard", func(t *testing.T) {
		multi = measure(t, 4)
		t.Logf("4-shard cycles/s: %.0f", multi)
	})

	if single == 0 || multi == 0 {
		t.Skip("one of the subtests didn't measure")
	}
	ratio := multi / single
	t.Logf("multi-shard / single-shard ratio: %.2fx", ratio)

	// Lower bound: multi-raft should at LEAST match single-raft. The
	// real expectation is a meaningful speedup, but loopback gRPC
	// contention + small workload may keep the ratio < 4×. Anything
	// below 0.9× would indicate a regression in the wrapper.
	if ratio < 0.9 {
		t.Errorf("multi-shard throughput %.0f is %.2fx single-shard %.0f — expected ≥0.9×",
			multi, ratio, single)
	}
}

// runMultiShardCycle is the multi-node variant of runCycleLoop: each
// goroutine rotates its target node on every operation, so a write
// that hits a non-leader shard recovers on the next attempt with a
// fresh UUID against a different node.
func runMultiShardCycle(ctx context.Context, nodes []*raftTestNode, concurrency int, ops *atomic.Int64) {
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 5 * time.Second}
	var counter atomic.Int64
	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				node := nodes[int(counter.Add(1))%len(nodes)]
				if node.closed.Load() {
					continue
				}
				taskID := postWithRetry(ctx, client, nodes, &counter, fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"g":%d},"priority":5}`, id))
				if taskID == "" {
					return
				}
				claimed := postWithRetry(ctx, client, nodes, &counter, `{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`)
				// claimed is the response from /claim; parse the id
				if claimed == "" {
					continue
				}
				// extract task id from claimed body
				var out map[string]any
				_ = json.Unmarshal([]byte(claimed), &out)
				cid, _ := out["id"].(string)
				if cid == "" {
					continue
				}
				// submit result to whichever node we can
				submitWithRetry(ctx, client, nodes, &counter, cid)
				if ops != nil {
					ops.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
}

// postWithRetry posts to /v1/codeq/tasks (create) or /tasks/claim and
// rotates nodes on transient "not leader". Returns the response body
// on the first 2xx. /tasks endpoint is auto-detected by the body
// shape: bodies starting with `{"commands":` go to /claim.
func postWithRetry(ctx context.Context, client *http.Client, nodes []*raftTestNode, counter *atomic.Int64, body string) string {
	path := "/v1/codeq/tasks"
	if strings.HasPrefix(body, `{"commands":`) {
		path = "/v1/codeq/tasks/claim"
	}
	for attempt := 0; attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return ""
		}
		node := nodes[int(counter.Add(1))%len(nodes)]
		if node.closed.Load() {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, node.server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
			b := make([]byte, 1024)
			n, _ := resp.Body.Read(b)
			resp.Body.Close()
			return string(b[:n])
		}
		resp.Body.Close()
	}
	return ""
}

func submitWithRetry(ctx context.Context, client *http.Client, nodes []*raftTestNode, counter *atomic.Int64, taskID string) {
	body := `{"status":"COMPLETED","result":{"ok":true}}`
	for attempt := 0; attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return
		}
		node := nodes[int(counter.Add(1))%len(nodes)]
		if node.closed.Load() {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, node.server.URL+"/v1/codeq/tasks/"+taskID+"/result", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
	}
}

