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
)

// TestRaftProfile_4Shard captures CPU + mutex + block + alloc
// profiles of a 3-node × 4-shard raft cluster under load. Used to
// hunt down why multi-shard doesn't scale over single-shard (the
// smart-routing bench shows ~1.0× ratio). Profiles land in
// /tmp/codeq-raft-profiles/.
//
// Run with:
//   go test -v -run='^TestRaftProfile_4Shard$' -count=1 -timeout=180s ./pkg/app
//
// Then:
//   go tool pprof -top -cum /tmp/codeq-raft-profiles/cpu.pb.gz
//   go tool pprof -top -cum /tmp/codeq-raft-profiles/mutex.pb.gz
//   go tool pprof -top -cum /tmp/codeq-raft-profiles/block.pb.gz
//   go tool pprof -alloc_space -top -cum /tmp/codeq-raft-profiles/alloc.pb.gz
//
// Skipped under -short.
func TestRaftProfile_4Shard(t *testing.T) {
	if testing.Short() {
		t.Skip("profile bench is long; run without -short")
	}

	const (
		warmup     = 2 * time.Second
		window     = 15 * time.Second
		concurrent = 32
		numNodes   = 3
		numShards  = 4
	)

	outDir := "/tmp/codeq-raft-profiles"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	nodes, cleanup := startSmartCluster(t, numNodes, numShards)
	t.Cleanup(cleanup)

	// Warmup so every shard has a leader before the measurement window
	// opens. Otherwise the first second of the profile is dominated by
	// elections.
	_ = smartSubmit(t, nodes, 16, 5*time.Second)

	wctx, wcancel := context.WithTimeout(context.Background(), warmup)
	runSmartCycle(wctx, nodes, concurrent, nil)
	wcancel()

	// Block + mutex profile rates: full sampling (rate=1). Restore on
	// exit so subsequent tests aren't slowed.
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

	var ops atomic.Int64
	mctx, mcancel := context.WithTimeout(context.Background(), window)
	start := time.Now()
	runSmartCycle(mctx, nodes, concurrent, &ops)
	mcancel()
	elapsed := time.Since(start)

	pprof.StopCPUProfile()

	rate := float64(ops.Load()) / elapsed.Seconds()
	t.Logf("PROFILE WINDOW (%v): %d cycles, %.0f cycles/s", elapsed, ops.Load(), rate)

	// Snapshot remaining profiles.
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
	t.Logf("Next:")
	t.Logf("  go tool pprof -top -cum %s/cpu.pb.gz", outDir)
	t.Logf("  go tool pprof -top -cum %s/mutex.pb.gz", outDir)
	t.Logf("  go tool pprof -top -cum %s/block.pb.gz", outDir)
	t.Logf("  go tool pprof -alloc_space -top -cum %s/allocs.pb.gz", outDir)
}
