package bench

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/producerclient"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// TestProfile_FullCycle drives the full single-node cycle (producer
// stream + worker stream) at saturation for a fixed window and writes
// CPU + alloc + block + mutex pprof profiles to /tmp/codeq-profiles.
// Use go tool pprof to read them:
//
//	go tool pprof -top -cum /tmp/codeq-profiles/cpu.pb.gz
//	go tool pprof -alloc_space -top -cum /tmp/codeq-profiles/alloc.pb.gz
//	go tool pprof -top -cum /tmp/codeq-profiles/block.pb.gz
//	go tool pprof -top -cum /tmp/codeq-profiles/mutex.pb.gz
//
// Run with:
//
//	go test -v -run='^TestProfile_FullCycle' -count=1 -timeout=180s ./internal/bench/...
//
// Skip with -short.
func TestProfile_FullCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("profile is long; run without -short")
	}

	outDir := "/tmp/codeq-profiles"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	streamProducerAddr := fmt.Sprintf("127.0.0.1:%d", freePortT(t))
	streamWorkerAddr := fmt.Sprintf("127.0.0.1:%d", freePortT(t))

	// Boot a single-node Application with worker stream enabled. We then
	// patch in the producer-stream addr via a second fixture call — both
	// helpers share the same Application via their cleanup hooks. The
	// producer fixture is the canonical "everything wired" boot path:
	// it accepts producer stream addr explicitly.
	_ = newPebbleAppForProducerWithWorker(t, streamProducerAddr, streamWorkerAddr)

	// Producer side: 32 goroutines pumping creates through the stream.
	prodCli, err := producerclient.New(producerclient.Config{
		Addr:  streamProducerAddr,
		Token: phase2ProducerToken,
	})
	if err != nil {
		t.Fatalf("producerclient.New: %v", err)
	}
	defer prodCli.Close()

	prodCtx, prodCancel := context.WithCancel(t.Context())
	defer prodCancel()
	prodSess, err := prodCli.Connect(prodCtx)
	if err != nil {
		t.Fatalf("producer Connect: %v", err)
	}
	defer prodSess.Close()

	// Worker side: workerclient with high concurrency to drain. BatchSize
	// is opt-in via env so the same harness can profile both single-task
	// (PHASE6_BATCH=0) and batched (PHASE6_BATCH=N) paths.
	batchSize, _ := strconv.Atoi(os.Getenv("PHASE6_BATCH"))
	workerCli, err := workerclient.New(workerclient.Config{
		Addr:         streamWorkerAddr,
		Token:        phase2WorkerToken,
		Commands:     []string{"GENERATE_MASTER"},
		Concurrency:  128,
		BatchSize:    batchSize,
		LeaseSeconds: 300,
	})
	if err != nil {
		t.Fatalf("workerclient.New: %v", err)
	}
	defer workerCli.Close()

	// --- Profiling setup ---

	// Block + mutex profile rates are global runtime knobs; restore on exit
	// to avoid polluting later tests in the same binary. Lower rates
	// (sample less often) reduce overhead — full sampling (1) is fine
	// for a 20s window.
	runtime.SetBlockProfileRate(1)
	defer runtime.SetBlockProfileRate(0)
	prevMutex := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(prevMutex)

	// Warm up for 2s — JIT-style: let queue channels fill, validators cache
	// keys, Pebble compact a bit. Profile starts AFTER warm-up so the
	// first few hundred ms of cold-start are not in the sample.
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	startProducerLoad(t, prodSess, warmupCtx, 16)
	startWorkerLoad(t, workerCli, warmupCtx, 64)
	<-warmupCtx.Done()
	warmupCancel()

	// --- Measurement window ---
	const window = 20 * time.Second
	measureCtx, measureCancel := context.WithTimeout(context.Background(), window)

	// CPU profile spans the entire window.
	cpuFile, err := os.Create(outDir + "/cpu.pb.gz")
	if err != nil {
		t.Fatalf("create cpu profile: %v", err)
	}
	defer cpuFile.Close()
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Fatalf("start cpu profile: %v", err)
	}

	var created, completed atomic.Int64
	createdCounter := startProducerLoad(t, prodSess, measureCtx, 32)
	completedCounter := startWorkerLoad(t, workerCli, measureCtx, 128)

	start := time.Now()
	<-measureCtx.Done()
	elapsed := time.Since(start)
	measureCancel()

	pprof.StopCPUProfile()

	// Snapshot heap-alloc, block, mutex AFTER the window so they reflect
	// what was accumulated during measurement.
	if err := writeProfile(outDir+"/alloc.pb.gz", "allocs"); err != nil {
		t.Fatalf("write alloc profile: %v", err)
	}
	if err := writeProfile(outDir+"/heap.pb.gz", "heap"); err != nil {
		t.Fatalf("write heap profile: %v", err)
	}
	if err := writeProfile(outDir+"/block.pb.gz", "block"); err != nil {
		t.Fatalf("write block profile: %v", err)
	}
	if err := writeProfile(outDir+"/mutex.pb.gz", "mutex"); err != nil {
		t.Fatalf("write mutex profile: %v", err)
	}
	if err := writeProfile(outDir+"/goroutine.pb.gz", "goroutine"); err != nil {
		t.Fatalf("write goroutine profile: %v", err)
	}

	created.Store(createdCounter.Load())
	completed.Store(completedCounter.Load())

	createRate := float64(created.Load()) / elapsed.Seconds()
	completeRate := float64(completed.Load()) / elapsed.Seconds()

	t.Logf("PROFILE WINDOW (%s):", elapsed.Round(time.Millisecond))
	t.Logf("  created   = %-10d (%.0f/s)", created.Load(), createRate)
	t.Logf("  completed = %-10d (%.0f/s)", completed.Load(), completeRate)
	t.Logf("PROFILES written to %s/", outDir)
	t.Logf("Next: go tool pprof -top -cum %s/cpu.pb.gz", outDir)
}

func writeProfile(path, kind string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	p := pprof.Lookup(kind)
	if p == nil {
		return fmt.Errorf("unknown profile %q", kind)
	}
	return p.WriteTo(f, 0)
}

// startProducerLoad launches `n` goroutines that pump creates through
// the producer session until ctx is cancelled. Returns the counter that
// tracks accepted creates.
func startProducerLoad(t *testing.T, sess *producerclient.Session, ctx context.Context, n int) *atomic.Int64 {
	t.Helper()
	body := []byte(`{"bench":true}`)
	var created atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for ctx.Err() == nil {
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
	// We don't wait for these goroutines; ctx cancel returns them.
	go func() { wg.Wait() }()
	return &created
}

// startWorkerLoad runs workerclient.Run with a counting handler until
// ctx is cancelled. Returns the counter for completed tasks. The
// `concurrency` argument is ignored — workerclient.Config.Concurrency
// owns the slot count for the session.
func startWorkerLoad(t *testing.T, cli *workerclient.Client, ctx context.Context, _ int) *atomic.Int64 {
	t.Helper()
	var completed atomic.Int64
	go func() {
		_ = cli.Run(ctx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
			completed.Add(1)
			return workerclient.Completed(nil)
		})
	}()
	return &completed
}
