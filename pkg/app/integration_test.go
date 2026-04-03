package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks" // Register JWKS provider
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestHTTPIntegrationFlow(t *testing.T) {
	ctx := context.Background()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key gen: %v", err)
	}
	const kid = "test-kid"
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		eBytes := []byte{0x01, 0x00, 0x01}
		e := base64.RawURLEncoding.EncodeToString(eBytes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"kty": "RSA", "kid": kid, "n": n, "e": e}},
		})
	}))
	t.Cleanup(jwksSrv.Close)

	callbackCh := make(chan map[string]any, 1)
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		select {
		case callbackCh <- payload:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hookSrv.Close)

	cfg := &config.Config{
		Port:                               0,
		RedisAddr:                          mr.Addr(),
		IdentityJwksURL:                    jwksSrv.URL,
		IdentityIssuer:                     "codeq-test",
		IdentityAudience:                   "codeq-producer",
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		Env:                                "test",
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  "/tmp/codeq-artifacts-test",
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      "fixed",
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WorkerJwksURL:                      jwksSrv.URL,
		WorkerAudience:                     "codeq-worker",
		WorkerIssuer:                       "codeq-test",
		AllowedClockSkewSeconds:            60,
		WebhookHmacSecret:                  "secret",
		SubscriptionMinIntervalSeconds:     5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:           3,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
	}

	// Setup auth providers config (normally done by LoadConfig)
	cfg.ProducerAuthProvider = "jwks"
	cfg.ProducerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.IdentityJwksURL,
		"issuer":      cfg.IdentityIssuer,
		"audience":    cfg.IdentityAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})
	cfg.WorkerAuthProvider = "jwks"
	cfg.WorkerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.WorkerJwksURL,
		"issuer":      cfg.WorkerIssuer,
		"audience":    cfg.WorkerAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})

	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validate: %v", err)
	}

	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("app init: %v", err)
	}
	SetupMappings(app)
	server := httptest.NewServer(app.Engine)
	t.Cleanup(server.Close)

	workerToken := signWorkerJWT(t, privKey, kid, "codeq-test", "codeq-worker", "worker-1")
	producerToken := signProducerJWT(t, privKey, kid, "codeq-test", "codeq-producer", "user-1")

	taskID := createTask(t, ctx, server.URL, producerToken, hookSrv.URL)
	claimTask(t, ctx, server.URL, workerToken, taskID)
	heartbeatTask(t, ctx, server.URL, workerToken, taskID)
	nackTask(t, ctx, server.URL, workerToken, taskID)
	claimTask(t, ctx, server.URL, workerToken, taskID)
	submitResult(t, ctx, server.URL, workerToken, taskID)
	getResult(t, ctx, server.URL, producerToken, taskID)
	assertMetricsExposed(t, server.URL)

	select {
	case payload := <-callbackCh:
		if payload["taskId"] != taskID {
			t.Fatalf("callback taskId mismatch: %v", payload["taskId"])
		}
		if payload["status"] != string(domain.StatusCompleted) {
			t.Fatalf("callback status mismatch: %v", payload["status"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected webhook callback")
	}
}

func assertMetricsExposed(t *testing.T, baseURL string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status %d body=%s", resp.StatusCode, string(b))
	}
	body := string(b)
	if !strings.Contains(body, `codeq_queue_depth{command="GENERATE_MASTER",queue="ready"}`) {
		t.Fatalf("expected queue depth metric in /metrics output")
	}
	if !strings.Contains(body, `codeq_task_created_total{command="GENERATE_MASTER"}`) {
		t.Fatalf("expected task created metric in /metrics output")
	}
	if !strings.Contains(body, `codeq_task_claimed_total{command="GENERATE_MASTER"}`) {
		t.Fatalf("expected task claimed metric in /metrics output")
	}
	if !strings.Contains(body, `codeq_task_completed_total{command="GENERATE_MASTER",status="COMPLETED"}`) {
		t.Fatalf("expected task completed metric in /metrics output")
	}
}

func signWorkerJWT(t *testing.T, key *rsa.PrivateKey, kid, iss, aud, sub string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	now := time.Now().Unix()
	payload := map[string]any{
		"iss":        iss,
		"aud":        aud,
		"sub":        sub,
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"tenantId":   "test-tenant",
		"eventTypes": []string{string(domain.CmdGenerateMaster)},
		"scope":      "codeq:claim codeq:heartbeat codeq:abandon codeq:nack codeq:result codeq:subscribe",
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(payload)
	signingInput := h + "." + p
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	s := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + s
}

func signProducerJWT(t *testing.T, key *rsa.PrivateKey, kid, iss, aud, sub string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	now := time.Now().Unix()
	payload := map[string]any{
		"iss":      iss,
		"aud":      aud,
		"sub":      sub,
		"exp":      now + 3600,
		"iat":      now - 10,
		"jti":      "jid-1",
		"tenantId": "test-tenant",
		"email":    "test@codeq.local",
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(payload)
	signingInput := h + "." + p
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	s := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + s
}

func signAdminJWT(t *testing.T, key *rsa.PrivateKey, kid, iss, aud, sub string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	now := time.Now().Unix()
	payload := map[string]any{
		"iss":      iss,
		"aud":      aud,
		"sub":      sub,
		"exp":      now + 3600,
		"iat":      now - 10,
		"jti":      "jid-admin",
		"tenantId": "test-tenant",
		"email":    "admin@codeq.local",
		"role":     "ADMIN",
		"scope":    "codeq:admin",
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(payload)
	signingInput := h + "." + p
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	s := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + s
}

func createTask(t *testing.T, ctx context.Context, baseURL, token, webhook string) string {
	t.Helper()
	body := map[string]any{
		"command":  string(domain.CmdGenerateMaster),
		"payload":  map[string]any{"foo": "bar"},
		"priority": 5,
		"webhook":  webhook,
	}
	var resp domain.Task
	status, bodyStr := doJSON(t, ctx, http.MethodPost, baseURL+"/v1/codeq/tasks", token, body, &resp)
	if status != http.StatusAccepted {
		t.Fatalf("create task status %d body=%s", status, bodyStr)
	}
	if resp.ID == "" {
		t.Fatalf("missing task id")
	}
	return resp.ID
}

func claimTask(t *testing.T, ctx context.Context, baseURL, token, taskID string) {
	t.Helper()
	body := map[string]any{
		"commands":     []string{string(domain.CmdGenerateMaster)},
		"leaseSeconds": 60,
		"waitSeconds":  1,
	}
	var resp domain.Task
	status, bodyStr := doJSON(t, ctx, http.MethodPost, baseURL+"/v1/codeq/tasks/claim", token, body, &resp)
	if status != http.StatusOK {
		t.Fatalf("claim status %d body=%s", status, bodyStr)
	}
	if resp.ID != taskID {
		t.Fatalf("claimed id mismatch: %s", resp.ID)
	}
}

func heartbeatTask(t *testing.T, ctx context.Context, baseURL, token, taskID string) {
	t.Helper()
	body := map[string]any{"extendSeconds": 60}
	status, bodyStr := doJSON(t, ctx, http.MethodPost, baseURL+"/v1/codeq/tasks/"+taskID+"/heartbeat", token, body, nil)
	if status != http.StatusOK {
		t.Fatalf("heartbeat status %d body=%s", status, bodyStr)
	}
}

func nackTask(t *testing.T, ctx context.Context, baseURL, token, taskID string) {
	t.Helper()
	body := map[string]any{"delaySeconds": 0, "reason": "TEST"}
	status, bodyStr := doJSON(t, ctx, http.MethodPost, baseURL+"/v1/codeq/tasks/"+taskID+"/nack", token, body, nil)
	if status != http.StatusOK {
		t.Fatalf("nack status %d body=%s", status, bodyStr)
	}
}

func submitResult(t *testing.T, ctx context.Context, baseURL, token, taskID string) {
	t.Helper()
	body := map[string]any{"status": string(domain.StatusCompleted), "result": map[string]any{"ok": true}}
	status, bodyStr := doJSON(t, ctx, http.MethodPost, baseURL+"/v1/codeq/tasks/"+taskID+"/result", token, body, nil)
	if status != http.StatusOK {
		t.Fatalf("result status %d body=%s", status, bodyStr)
	}
}

func getResult(t *testing.T, ctx context.Context, baseURL, token, taskID string) {
	t.Helper()
	var resp struct {
		Task   domain.Task         `json:"task"`
		Result domain.ResultRecord `json:"result"`
	}
	status, bodyStr := doJSON(t, ctx, http.MethodGet, baseURL+"/v1/codeq/tasks/"+taskID+"/result", token, nil, &resp)
	if status != http.StatusOK {
		t.Fatalf("get result status %d body=%s", status, bodyStr)
	}
	if resp.Result.TaskID != taskID {
		t.Fatalf("result taskId mismatch: %v", resp.Result.TaskID)
	}
}

func doJSON(t *testing.T, ctx context.Context, method, url, token string, body any, out any) (int, string) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, string(b)
}

// TestHTTPIntegrationFlow_Sharded verifies that the full task lifecycle works
// correctly when queue sharding is enabled with multiple Redis backends.
// It tests both shard routing and cross-shard admin aggregation.
func TestHTTPIntegrationFlow_Sharded(t *testing.T) {
	ctx := context.Background()

	// Create two separate miniredis instances simulating two shard backends
	mrPrimary, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis primary: %v", err)
	}
	t.Cleanup(mrPrimary.Close)

	mrCompute, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis compute: %v", err)
	}
	t.Cleanup(mrCompute.Close)

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key gen: %v", err)
	}
	const kid = "test-kid-shard"
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		eBytes := []byte{0x01, 0x00, 0x01}
		e := base64.RawURLEncoding.EncodeToString(eBytes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"kty": "RSA", "kid": kid, "n": n, "e": e}},
		})
	}))
	t.Cleanup(jwksSrv.Close)

	cfg := &config.Config{
		Port:                               0,
		RedisAddr:                          mrPrimary.Addr(),
		IdentityJwksURL:                    jwksSrv.URL,
		IdentityIssuer:                     "codeq-test",
		IdentityAudience:                   "codeq-producer",
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		Env:                                "test",
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  "/tmp/codeq-artifacts-test-shard",
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      "fixed",
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WorkerJwksURL:                      jwksSrv.URL,
		WorkerAudience:                     "codeq-worker",
		WorkerIssuer:                       "codeq-test",
		AllowedClockSkewSeconds:            60,
		WebhookHmacSecret:                  "secret",
		SubscriptionMinIntervalSeconds:     5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:           3,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
		Sharding: config.ShardingConfig{
			Enabled:      true,
			DefaultShard: "primary",
			CommandMappings: map[string]string{
				"GENERATE_MASTER": "compute",
			},
			TenantOverrides: map[string]string{},
			Backends: map[string]config.ShardBackendConfig{
				"primary": {Address: mrPrimary.Addr(), PoolSize: 5},
				"compute": {Address: mrCompute.Addr(), PoolSize: 5},
			},
		},
	}

	cfg.ProducerAuthProvider = "jwks"
	cfg.ProducerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.IdentityJwksURL,
		"issuer":      cfg.IdentityIssuer,
		"audience":    cfg.IdentityAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})
	cfg.WorkerAuthProvider = "jwks"
	cfg.WorkerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.WorkerJwksURL,
		"issuer":      cfg.WorkerIssuer,
		"audience":    cfg.WorkerAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})

	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validate: %v", err)
	}

	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("app init: %v", err)
	}
	SetupMappings(app)
	server := httptest.NewServer(app.Engine)
	t.Cleanup(server.Close)

	workerToken := signWorkerJWT(t, privKey, kid, "codeq-test", "codeq-worker", "worker-1")
	producerToken := signProducerJWT(t, privKey, kid, "codeq-test", "codeq-producer", "user-1")

	// Create task (GENERATE_MASTER → compute shard)
	taskID := createTask(t, ctx, server.URL, producerToken, "")

	// Verify task data is on compute shard, NOT primary
	computeClient := redis.NewClient(&redis.Options{Addr: mrCompute.Addr()})
	t.Cleanup(func() { _ = computeClient.Close() })
	primaryClient := redis.NewClient(&redis.Options{Addr: mrPrimary.Addr()})
	t.Cleanup(func() { _ = primaryClient.Close() })

	taskJSON, err := computeClient.HGet(ctx, "codeq:tasks", taskID).Result()
	if err != nil || taskJSON == "" {
		t.Fatalf("expected task on compute shard, got err=%v", err)
	}
	_, err = primaryClient.HGet(ctx, "codeq:tasks", taskID).Result()
	if err == nil {
		t.Fatal("task should NOT be on primary shard")
	}

	// Task operations through sharded repository: claim → heartbeat → nack → claim
	claimTask(t, ctx, server.URL, workerToken, taskID)
	heartbeatTask(t, ctx, server.URL, workerToken, taskID)
	nackTask(t, ctx, server.URL, workerToken, taskID)
	claimTask(t, ctx, server.URL, workerToken, taskID)

	// Verify admin queues endpoint works with sharding (aggregates across shards)
	adminToken := signAdminJWT(t, privKey, kid, "codeq-test", "codeq-producer", "admin-1")
	var queues map[string]any
	status, body := doJSON(t, ctx, http.MethodGet, server.URL+"/v1/codeq/admin/queues", adminToken, nil, &queues)
	if status != http.StatusOK {
		t.Fatalf("admin queues status %d body=%s", status, body)
	}

	// Verify queue stats endpoint works for sharded command
	var stats map[string]any
	status, body = doJSON(t, ctx, http.MethodGet, server.URL+"/v1/codeq/admin/queues/GENERATE_MASTER", adminToken, nil, &stats)
	if status != http.StatusOK {
		t.Fatalf("queue stats status %d body=%s", status, body)
	}

	// Verify GET task works (fans out to correct shard)
	var task domain.Task
	status, body = doJSON(t, ctx, http.MethodGet, server.URL+"/v1/codeq/tasks/"+taskID, producerToken, nil, &task)
	if status != http.StatusOK {
		t.Fatalf("get task status %d body=%s", status, body)
	}
	if task.ID != taskID {
		t.Fatalf("get task returned wrong id: %s", task.ID)
	}
}
