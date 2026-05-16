package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// TestReproDelayedResultFailures reproduces the k6 scenario 06 finding
// (6% fail rate on POST /tasks/:id/result) in-process so we can capture
// the exact status-code distribution and identify which error the
// server is returning. Run with:
//
//	go test -v -run='^TestReproDelayedResultFailures' -count=1 -timeout=180s ./internal/bench/...
func TestReproDelayedResultFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("repro is long; run without -short")
	}
	gin.SetMode(gin.ReleaseMode)

	producerCfg, _ := json.Marshal(map[string]any{
		"token":   phase2ProducerToken,
		"subject": "producer-repro",
		"raw":     map[string]any{"role": "ADMIN", "tenantId": phase2Tenant},
	})
	workerCfg, _ := json.Marshal(map[string]any{
		"token":      phase2WorkerToken,
		"subject":    "worker-repro",
		"scopes":     []string{"codeq:claim", "codeq:heartbeat", "codeq:abandon", "codeq:nack", "codeq:result", "codeq:subscribe"},
		"eventTypes": []string{"*"},
		"raw":        map[string]any{"tenantId": phase2Tenant},
	})
	pebbleCfg, _ := json.Marshal(map[string]any{
		"path":          t.TempDir(),
		"fsyncOnCommit": false,
	})

	cfg := &config.Config{
		Env:                                "dev",
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                200,
		LocalArtifactsDir:                  t.TempDir(),
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      "fixed",
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WebhookHmacSecret:                  "repro-secret",
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
		RateLimit:                          config.RateLimitConfig{},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate cfg: %v", err)
	}
	a, err := app.NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	app.SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.TracingShutdown(ctx)
	})

	// Configuration mirrors k6 06_delayed_tasks.js
	const (
		runDuration = 30 * time.Second
		producerVUs = 32
		workerVUs   = 100
		delayPct    = 50
		minDelay    = 1
		maxDelay    = 10 // shorter than k6 (30) to keep test bounded
	)
	ctx, cancel := context.WithTimeout(t.Context(), runDuration+10*time.Second)
	defer cancel()

	deadline := time.Now().Add(runDuration)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	var (
		created   atomic.Int64
		claimed   atomic.Int64
		ok200     atomic.Int64
		fail400   atomic.Int64
		fail401   atomic.Int64
		fail403   atomic.Int64
		fail404   atomic.Int64
		fail409   atomic.Int64
		failOther atomic.Int64
	)

	type errSample struct {
		status int
		body   string
		taskID string
	}
	samplesMu := sync.Mutex{}
	samples := make(map[int][]errSample)

	captureErr := func(status int, taskID string, body []byte) {
		samplesMu.Lock()
		defer samplesMu.Unlock()
		s := samples[status]
		if len(s) < 3 {
			samples[status] = append(s, errSample{status, string(body), taskID})
		}
	}

	// Producer goroutines.
	prodCtx, prodCancel := context.WithCancel(ctx)
	defer prodCancel()
	rng := rand.New(rand.NewSource(1))
	var rngMu sync.Mutex

	for range producerVUs {
		go func() {
			for {
				if time.Now().After(deadline) || prodCtx.Err() != nil {
					return
				}
				rngMu.Lock()
				isDelayed := rng.Intn(100) < delayPct
				delaySeconds := 0
				if isDelayed {
					delaySeconds = minDelay + rng.Intn(maxDelay-minDelay+1)
				}
				rngMu.Unlock()

				body := map[string]any{
					"command": "GENERATE_MASTER",
					"payload": map[string]any{"delayed": isDelayed},
				}
				if delaySeconds > 0 {
					body["delaySeconds"] = delaySeconds
				}
				bb, _ := json.Marshal(body)
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks", bytes.NewReader(bb))
				req.Header.Set("Authorization", "Bearer "+phase2ProducerToken)
				req.Header.Set("Content-Type", "application/json")
				resp, err := httpClient.Do(req)
				if err != nil {
					continue
				}
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusAccepted {
					created.Add(1)
				}
			}
		}()
	}

	// Worker goroutines.
	var wg sync.WaitGroup
	for range workerVUs {
		wg.Go(func() {
			for {
				if time.Now().After(deadline) || ctx.Err() != nil {
					return
				}
				// Claim
				claimReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/claim",
					strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60}`))
				claimReq.Header.Set("Authorization", "Bearer "+phase2WorkerToken)
				claimReq.Header.Set("Content-Type", "application/json")
				claimResp, err := httpClient.Do(claimReq)
				if err != nil {
					continue
				}
				if claimResp.StatusCode == http.StatusNoContent {
					_ = claimResp.Body.Close()
					time.Sleep(50 * time.Millisecond)
					continue
				}
				if claimResp.StatusCode != http.StatusOK {
					_ = claimResp.Body.Close()
					continue
				}
				var t struct {
					ID string `json:"id"`
				}
				_ = json.NewDecoder(claimResp.Body).Decode(&t)
				_ = claimResp.Body.Close()
				claimed.Add(1)
				if t.ID == "" {
					continue
				}

				// Submit result
				resBody := strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`)
				resReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/"+t.ID+"/result", resBody)
				resReq.Header.Set("Authorization", "Bearer "+phase2WorkerToken)
				resReq.Header.Set("Content-Type", "application/json")
				resResp, err := httpClient.Do(resReq)
				if err != nil {
					continue
				}
				body, _ := io.ReadAll(resResp.Body)
				_ = resResp.Body.Close()
				switch resResp.StatusCode {
				case http.StatusOK:
					ok200.Add(1)
				case http.StatusBadRequest:
					fail400.Add(1)
					captureErr(400, t.ID, body)
				case http.StatusUnauthorized:
					fail401.Add(1)
					captureErr(401, t.ID, body)
				case http.StatusForbidden:
					fail403.Add(1)
					captureErr(403, t.ID, body)
				case http.StatusNotFound:
					fail404.Add(1)
					captureErr(404, t.ID, body)
				case http.StatusConflict:
					fail409.Add(1)
					captureErr(409, t.ID, body)
				default:
					failOther.Add(1)
					captureErr(resResp.StatusCode, t.ID, body)
				}
			}
		})
	}

	wg.Wait()
	prodCancel()

	total := ok200.Load() + fail400.Load() + fail401.Load() + fail403.Load() + fail404.Load() + fail409.Load() + failOther.Load()
	failed := total - ok200.Load()
	failPct := 0.0
	if total > 0 {
		failPct = 100 * float64(failed) / float64(total)
	}
	t.Logf("=== REPRO REPORT ===")
	t.Logf("created=%d  claimed=%d  result_total=%d  result_ok=%d  result_failed=%d (%.2f%%)",
		created.Load(), claimed.Load(), total, ok200.Load(), failed, failPct)
	t.Logf("status breakdown: 400=%d  401=%d  403=%d  404=%d  409=%d  other=%d",
		fail400.Load(), fail401.Load(), fail403.Load(), fail404.Load(), fail409.Load(), failOther.Load())

	samplesMu.Lock()
	defer samplesMu.Unlock()
	for code, ss := range samples {
		for i, s := range ss {
			t.Logf("sample %d/%d status=%d taskID=%s body=%s",
				i+1, len(ss), code, s.taskID, strings.TrimSpace(s.body))
		}
	}

	// Allow the slog "DEFAULT" stderr to flush.
	_ = fmt.Sprintf("")
}
