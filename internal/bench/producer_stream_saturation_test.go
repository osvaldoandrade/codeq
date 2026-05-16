package bench

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/producerclient"
)

// TestSaturation_ProducerStreamPath sweeps producer Concurrency to find
// the per-process create throughput ceiling.
//
//	go test -run='^TestSaturation_ProducerStreamPath' -count=1 -timeout=300s ./internal/bench/...
func TestSaturation_ProducerStreamPath(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation test is long; run without -short")
	}
	streamAddr := fmt.Sprintf("127.0.0.1:%d", freePortT(t))
	_ = newPebbleAppForProducer(t, streamAddr)

	cli, err := producerclient.New(producerclient.Config{
		Addr:  streamAddr,
		Token: phase2ProducerToken,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cli.Close()

	concurrencies := []int{1, 4, 16, 32, 64, 128, 256, 512}
	body := []byte(`{"bench":true}`)

	for _, c := range concurrencies {
		t.Run(fmt.Sprintf("concurrency=%d", c), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sess, err := cli.Connect(ctx)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			defer sess.Close()

			var created atomic.Int64
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for range c {
				wg.Go(func() {
					for {
						select {
						case <-stop:
							return
						default:
						}
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

			start := time.Now()
			time.Sleep(phase2RunDuration)
			close(stop)
			wg.Wait()
			elapsed := time.Since(start)
			rate := float64(created.Load()) / elapsed.Seconds()
			t.Logf("c=%-4d  created=%-7d  rate=%8.0f creates/s", c, created.Load(), rate)

			time.Sleep(100 * time.Millisecond)
		})
	}
}
