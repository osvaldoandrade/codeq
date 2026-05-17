package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// Phase 2 throughput tests: same Pebble-backed Application, same
// continuously-fed queue, two worker paths. Each path runs for a fixed
// wall-clock window and we report tasks completed per second.
//
//	go test -run='^TestThroughput' -count=1 -timeout=180s ./internal/bench/...
//
// These are regular Tests (not Benchmarks) because Go bench's b.N
// convergence interacts badly with a producer/consumer harness whose
// setup cost dominates small b.N runs.

const (
	phase2Tenant        = "bench-tenant-p2"
	phase2ProducerToken = "bench-producer-p2"
	phase2WorkerToken   = "bench-worker-p2"

	phase2WorkerConcurrency = 32
	phase2RunDuration       = 6 * time.Second
)

func freePortT(tb testing.TB) int {
	tb.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

func newPebbleAppForBench(tb testing.TB, streamAddr string) *httptest.Server {
	tb.Helper()
	gin.SetMode(gin.ReleaseMode)

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
	numShards, _ := strconv.Atoi(os.Getenv("PHASE8_SHARDS"))
	pebbleCfg, _ := json.Marshal(map[string]any{
		"path":          tb.TempDir(),
		"fsyncOnCommit": false,
		"numShards":     numShards,
	})

	cfg := &config.Config{
		Env:                                "dev",
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		DefaultLeaseSeconds:                300,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  tb.TempDir(),
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
		WorkerStreamAddr:                   streamAddr,
		RateLimit:                          config.RateLimitConfig{},
	}
	if err := cfg.Validate(); err != nil {
		tb.Fatalf("validate cfg: %v", err)
	}
	a, err := app.NewApplication(cfg)
	if err != nil {
		tb.Fatalf("NewApplication: %v", err)
	}
	app.SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	tb.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.TracingShutdown(ctx)
	})
	if streamAddr != "" {
		time.Sleep(150 * time.Millisecond)
	}
	return srv
}

// runProducer feeds the queue continuously until ctx is cancelled. The
// initial burst frontloads enough work so workers never see an empty
// queue at startup, then the producer keeps topping up at a high rate.
func runProducer(t *testing.T, ctx context.Context, baseURL string, initial int) (created *atomic.Int64) {
	t.Helper()
	created = &atomic.Int64{}
	client := &http.Client{Timeout: 10 * time.Second}
	payload := []byte(`{"command":"GENERATE_MASTER","payload":{"bench":true}}`)

	post := func() error {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/codeq/tasks", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+phase2ProducerToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("status=%d", resp.StatusCode)
		}
		return nil
	}

	// Initial burst — 16 goroutines fan out.
	var wg sync.WaitGroup
	jobs := make(chan struct{}, initial)
	for range initial {
		jobs <- struct{}{}
	}
	close(jobs)
	for range 16 {
		wg.Go(func() {
			for range jobs {
				if err := post(); err != nil {
					t.Errorf("prefill: %v", err)
					return
				}
				created.Add(1)
			}
		})
	}
	wg.Wait()

	// Background top-up. 32 goroutines keep the queue topped up at the
	// producer's HTTP ceiling so the worker side is what we measure.
	for range 32 {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := post(); err == nil {
					created.Add(1)
				}
			}
		}()
	}
	return created
}

func TestThroughput_RESTPath(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput tests are long; run without -short")
	}
	srv := newPebbleAppForBench(t, "")

	prodCtx, prodCancel := context.WithCancel(context.Background())
	created := runProducer(t, prodCtx, srv.URL, 5000)
	defer prodCancel()

	httpClient := &http.Client{Timeout: 10 * time.Second}
	var completed atomic.Int64
	stop := make(chan struct{})

	var wg sync.WaitGroup
	for range phase2WorkerConcurrency {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				claimReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/claim",
					strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":300}`))
				claimReq.Header.Set("Authorization", "Bearer "+phase2WorkerToken)
				claimReq.Header.Set("Content-Type", "application/json")
				claimResp, err := httpClient.Do(claimReq)
				if err != nil {
					return
				}
				if claimResp.StatusCode == http.StatusNoContent {
					_ = claimResp.Body.Close()
					continue
				}
				if claimResp.StatusCode != http.StatusOK {
					_ = claimResp.Body.Close()
					return
				}
				var got struct {
					ID string `json:"id"`
				}
				_ = json.NewDecoder(claimResp.Body).Decode(&got)
				_ = claimResp.Body.Close()

				resBody := strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`)
				resReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/"+got.ID+"/result", resBody)
				resReq.Header.Set("Authorization", "Bearer "+phase2WorkerToken)
				resReq.Header.Set("Content-Type", "application/json")
				resResp, err := httpClient.Do(resReq)
				if err != nil {
					return
				}
				_ = resResp.Body.Close()
				if resResp.StatusCode != http.StatusOK {
					return
				}
				completed.Add(1)
			}
		})
	}

	start := time.Now()
	time.Sleep(phase2RunDuration)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)
	prodCancel()

	rate := float64(completed.Load()) / elapsed.Seconds()
	t.Logf("REST: completed=%d created=%d duration=%s rate=%.0f tasks/s",
		completed.Load(), created.Load(), elapsed.Round(time.Millisecond), rate)
}

func TestThroughput_StreamPath(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput tests are long; run without -short")
	}
	streamAddr := fmt.Sprintf("127.0.0.1:%d", freePortT(t))
	srv := newPebbleAppForBench(t, streamAddr)

	prodCtx, prodCancel := context.WithCancel(context.Background())
	created := runProducer(t, prodCtx, srv.URL, 5000)
	defer prodCancel()

	cli, err := workerclient.New(workerclient.Config{
		Addr:         streamAddr,
		Token:        phase2WorkerToken,
		Commands:     []string{"GENERATE_MASTER"},
		Concurrency:  phase2WorkerConcurrency,
		LeaseSeconds: 300,
	})
	if err != nil {
		t.Fatalf("workerclient.New: %v", err)
	}
	defer cli.Close()

	var completed atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cli.Run(ctx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
			completed.Add(1)
			return workerclient.Completed(map[string]any{"ok": true})
		})
	}()

	start := time.Now()
	time.Sleep(phase2RunDuration)
	cancel()
	elapsed := time.Since(start)
	prodCancel()

	rate := float64(completed.Load()) / elapsed.Seconds()
	t.Logf("STREAM: completed=%d created=%d duration=%s rate=%.0f tasks/s",
		completed.Load(), created.Load(), elapsed.Round(time.Millisecond), rate)
}
