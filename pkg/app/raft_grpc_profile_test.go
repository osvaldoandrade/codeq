package app

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/producerclient"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// TestRaftProfile_GRPC4Shard captures CPU + mutex + block + alloc
// profiles of the 3-node × 4-shard raft cluster driven through the
// gRPC stream path (NOT the HTTP smart-routing path). Used to find
// the next gargalo after HTTP transport contention is removed.
//
// Profiles land in /tmp/codeq-raft-grpc-profiles/.
//
// Run with:
//
//	go test -v -run='^TestRaftProfile_GRPC4Shard$' -count=1 -timeout=180s ./pkg/app
//
// Then:
//
//	go tool pprof -top -cum /tmp/codeq-raft-grpc-profiles/cpu.pb.gz
//	go tool pprof -top -cum /tmp/codeq-raft-grpc-profiles/mutex.pb.gz
//	go tool pprof -top -cum /tmp/codeq-raft-grpc-profiles/block.pb.gz
//
// Skipped under -short.
func TestRaftProfile_GRPC4Shard(t *testing.T) {
	if testing.Short() {
		t.Skip("profile is long; run without -short")
	}

	const (
		warmup     = 2 * time.Second
		window     = 15 * time.Second
		concurrent = 64
		numNodes   = 3
		numShards  = 4
	)

	outDir := "/tmp/codeq-raft-grpc-profiles"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	nodes, cleanup := startGRPCCluster(t, numNodes, numShards)
	t.Cleanup(cleanup)

	// Per-node producer session + worker client.
	prodSess := make([]*producerclient.Session, numNodes)
	workerClis := make([]*workerclient.Client, numNodes)
	cancels := make([]context.CancelFunc, 0, numNodes)
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
		pcli, err := producerclient.New(producerclient.Config{
			Addr:  n.producerAddr,
			Token: "dev-token",
		})
		if err != nil {
			t.Fatalf("[%s] producerclient.New: %v", n.id, err)
		}
		pctx, pcancel := context.WithCancel(context.Background())
		cancels = append(cancels, pcancel)
		sess, err := pcli.Connect(pctx)
		if err != nil {
			t.Fatalf("[%s] Connect: %v", n.id, err)
		}
		prodSess[i] = sess

		wcli, err := workerclient.New(workerclient.Config{
			Addr:         n.workerAddr,
			Token:        "dev-token",
			Commands:     []string{"GENERATE_MASTER"},
			Concurrency:  128,
			LeaseSeconds: 60,
		})
		if err != nil {
			t.Fatalf("[%s] workerclient.New: %v", n.id, err)
		}
		workerClis[i] = wcli
	}

	_ = grpcWarmup(prodSess, 4*time.Second)
	wctx, wcancel := context.WithTimeout(context.Background(), warmup)
	runGRPCCycle(wctx, prodSess, workerClis, concurrent, 1, nil, nil)
	wcancel()

	runtime.SetBlockProfileRate(1)
	defer runtime.SetBlockProfileRate(0)
	prevMutex := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(prevMutex)

	cpuFile, err := os.Create(outDir + "/cpu.pb.gz")
	if err != nil {
		t.Fatalf("create cpu profile: %v", err)
	}
	defer cpuFile.Close()
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Fatalf("start cpu profile: %v", err)
	}

	var created, completed atomic.Int64
	mctx, mcancel := context.WithTimeout(context.Background(), window)
	start := time.Now()
	runGRPCCycle(mctx, prodSess, workerClis, concurrent, 1, &created, &completed)
	mcancel()
	elapsed := time.Since(start)
	pprof.StopCPUProfile()

	createRate := float64(created.Load()) / elapsed.Seconds()
	completeRate := float64(completed.Load()) / elapsed.Seconds()
	t.Logf("PROFILE WINDOW (%v):", elapsed)
	t.Logf("  created   = %d (%.0f/s)", created.Load(), createRate)
	t.Logf("  completed = %d (%.0f/s)", completed.Load(), completeRate)

	for _, name := range []string{"mutex", "block", "allocs", "goroutine"} {
		f, err := os.Create(fmt.Sprintf("%s/%s.pb.gz", outDir, name))
		if err != nil {
			t.Errorf("create %s profile: %v", name, err)
			continue
		}
		if p := pprof.Lookup(name); p != nil {
			if err := p.WriteTo(f, 0); err != nil {
				t.Errorf("write %s profile: %v", name, err)
			}
		}
		f.Close()
	}

	t.Logf("PROFILES written to %s/", outDir)
}
