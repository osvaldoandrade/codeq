package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
)

// Phase 3 throughput tests: same Pebble-backed Application, two
// producer paths. Each runs for a fixed window and reports creates/s.
//
//	go test -run='^TestProducerThroughput' -count=1 -timeout=180s ./internal/bench/...

const (
	phase3Concurrency = 32
	phase3Duration    = 6 * time.Second
)

func newPebbleAppForProducer(tb testing.TB, streamAddr string) *httptest.Server {
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
	pebbleCfg, _ := json.Marshal(map[string]any{
		"path":          tb.TempDir(),
		"fsyncOnCommit": false,
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
		ProducerStreamAddr:                 streamAddr,
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

func TestProducerThroughput_RESTPath(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput tests are long; run without -short")
	}
	srv := newPebbleAppForProducer(t, "")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	payload := []byte(`{"command":"GENERATE_MASTER","payload":{"bench":true}}`)
	var created atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range phase3Concurrency {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+phase2ProducerToken)
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpClient.Do(req)
				if err != nil {
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusAccepted {
					created.Add(1)
				}
			}
		})
	}

	start := time.Now()
	time.Sleep(phase3Duration)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)
	rate := float64(created.Load()) / elapsed.Seconds()
	t.Logf("REST: created=%d duration=%s rate=%.0f creates/s",
		created.Load(), elapsed.Round(time.Millisecond), rate)
}

func TestProducerThroughput_StreamPath(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput tests are long; run without -short")
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := cli.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	body := []byte(`{"bench":true}`)
	var created atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range phase3Concurrency {
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
	time.Sleep(phase3Duration)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)
	rate := float64(created.Load()) / elapsed.Seconds()
	t.Logf("STREAM: created=%d duration=%s rate=%.0f creates/s",
		created.Load(), elapsed.Round(time.Millisecond), rate)
}
