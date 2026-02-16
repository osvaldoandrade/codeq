package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static" // Register static auth provider.
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	benchTenant        = "bench-tenant"
	benchProducerToken = "bench-producer-token"
	benchWorkerToken   = "bench-worker-token"
	benchProducerSub   = "bench-producer"
	benchWorkerSub     = "bench-worker"
)

func newBenchApp(b *testing.B) *app.Application {
	b.Helper()
	gin.SetMode(gin.ReleaseMode)

	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis start: %v", err)
	}
	b.Cleanup(mr.Close)

	cfg := &config.Config{
		Env:                 "dev",
		Timezone:            "UTC",
		LogLevel:            "error",
		LogFormat:           "json",
		RedisAddr:           mr.Addr(),
		RedisPassword:       "",
		DefaultLeaseSeconds: 60,
		RequeueInspectLimit: 50,
		LocalArtifactsDir:   b.TempDir(),
		MaxAttemptsDefault:  5,
		BackoffPolicy:       "fixed",
		BackoffBaseSeconds:  1,
		BackoffMaxSeconds:   3,
		WebhookHmacSecret:   "bench-secret",

		ProducerAuthProvider: "static",
		WorkerAuthProvider:   "static",

		// Benchmarks keep rate limiting disabled.
		RateLimit: config.RateLimitConfig{},
	}

	producerAuthCfg, _ := json.Marshal(map[string]any{
		"token":   benchProducerToken,
		"subject": benchProducerSub,
		"email":   "bench@codeq.local",
		"raw": map[string]any{
			"role":     "ADMIN",
			"tenantId": benchTenant,
		},
	})
	workerAuthCfg, _ := json.Marshal(map[string]any{
		"token":   benchWorkerToken,
		"subject": benchWorkerSub,
		"scopes": []string{
			"codeq:claim",
			"codeq:heartbeat",
			"codeq:abandon",
			"codeq:nack",
			"codeq:result",
			"codeq:subscribe",
		},
		"eventTypes": []string{"*"},
		"raw": map[string]any{
			"tenantId": benchTenant,
		},
	})
	cfg.ProducerAuthConfig = producerAuthCfg
	cfg.WorkerAuthConfig = workerAuthCfg

	a, err := app.NewApplication(cfg)
	if err != nil {
		b.Fatalf("app init: %v", err)
	}
	app.SetupMappings(a)
	b.Cleanup(func() { _ = a.TracingShutdown(context.Background()) })
	return a
}

func doJSONRequest(b *testing.B, h http.Handler, method, path, bearerToken string, body []byte) (int, []byte) {
	b.Helper()

	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader([]byte{})
	} else {
		rbody = bytes.NewReader(body)
	}

	req := httptest.NewRequest(method, path, rbody)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func BenchmarkHTTP_CreateClaimComplete(b *testing.B) {
	a := newBenchApp(b)

	const prefill = 100
	createBody := []byte(`{"command":"GENERATE_MASTER","payload":{"bench":true},"priority":0}`)
	claimBody := []byte(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`)
	resultBody := []byte(`{"status":"COMPLETED","result":{"ok":true}}`)

	// Prefill queue to keep create -> notify path from dominating the benchmark (depth stays > 1).
	for i := 0; i < prefill; i++ {
		status, resp := doJSONRequest(b, a.Engine, http.MethodPost, "/v1/codeq/tasks", benchProducerToken, createBody)
		if status != http.StatusAccepted {
			b.Fatalf("prefill create status %d body=%s", status, string(resp))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		status, resp := doJSONRequest(b, a.Engine, http.MethodPost, "/v1/codeq/tasks", benchProducerToken, createBody)
		if status != http.StatusAccepted {
			b.Fatalf("create status %d body=%s", status, string(resp))
		}

		status, resp = doJSONRequest(b, a.Engine, http.MethodPost, "/v1/codeq/tasks/claim", benchWorkerToken, claimBody)
		if status != http.StatusOK {
			b.Fatalf("claim status %d body=%s", status, string(resp))
		}
		var claimed struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(resp, &claimed); err != nil || claimed.ID == "" {
			b.Fatalf("claim parse failed: err=%v body=%s", err, string(resp))
		}

		status, resp = doJSONRequest(b, a.Engine, http.MethodPost, "/v1/codeq/tasks/"+claimed.ID+"/result", benchWorkerToken, resultBody)
		if status != http.StatusOK {
			b.Fatalf("result status %d body=%s", status, string(resp))
		}
	}
}

func BenchmarkScheduler_CreateClaimComplete(b *testing.B) {
	a := newBenchApp(b)
	ctx := context.Background()

	const prefill = 100
	for i := 0; i < prefill; i++ {
		_, err := a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"bench":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)
		if err != nil {
			b.Fatalf("prefill CreateTask: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"bench":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)
		if err != nil {
			b.Fatalf("CreateTask: %v", err)
		}
		task, ok, err := a.Scheduler.ClaimTask(ctx, benchWorkerSub, []domain.Command{domain.CmdGenerateMaster}, 60, 0, benchTenant)
		if err != nil || !ok || task == nil {
			b.Fatalf("ClaimTask: ok=%v err=%v", ok, err)
		}

		_, err = a.Results.Submit(ctx, task.ID, domain.SubmitResultRequest{
			WorkerID: benchWorkerSub,
			Status:   domain.StatusCompleted,
			Result:   map[string]any{"ok": true},
		})
		if err != nil {
			b.Fatalf("Submit: %v", err)
		}
	}
}
