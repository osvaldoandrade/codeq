package bench

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// TestSaturation_StreamPath sweeps worker Concurrency to find the
// per-process stream throughput ceiling. Each step runs phase2RunDuration
// and we log the rate; the curve plateaus once we saturate the server's
// claim path or its Pebble write loop.
//
// Skip with -short. Run with:
//   go test -run='^TestSaturation_StreamPath' -count=1 -timeout=300s ./internal/bench/...
func TestSaturation_StreamPath(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation test is long; run without -short")
	}
	streamAddr := fmt.Sprintf("127.0.0.1:%d", freePortT(t))
	srv := newPebbleAppForBench(t, streamAddr)

	// One producer harness shared across all steps. Frontload 20k to
	// absorb the brief gap between client teardown and next step's start.
	prodCtx, prodCancel := context.WithCancel(t.Context())
	defer prodCancel()
	_ = runProducer(t, prodCtx, srv.URL, 20000)

	concurrencies := []int{1, 4, 16, 32, 64, 128, 256, 512}
	for _, c := range concurrencies {
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			cli, err := workerclient.New(workerclient.Config{
				Addr:         streamAddr,
				Token:        phase2WorkerToken,
				Commands:     []string{"GENERATE_MASTER"},
				Concurrency:  c,
				LeaseSeconds: 300,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer cli.Close()

			var completed atomic.Int64
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go func() {
				_ = cli.Run(ctx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
					completed.Add(1)
					return workerclient.Completed(nil)
				})
			}()

			start := time.Now()
			time.Sleep(phase2RunDuration)
			cancel()
			elapsed := time.Since(start)
			rate := float64(completed.Load()) / elapsed.Seconds()
			t.Logf("c=%-4d  completed=%-7d  rate=%8.0f tasks/s", c, completed.Load(), rate)

			// Allow client goroutines to wind down before next step
			// reaches into the same producer/server.
			time.Sleep(100 * time.Millisecond)
		})
	}
}
