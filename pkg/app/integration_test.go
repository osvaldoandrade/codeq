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
	"testing"
	"time"

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

	identitySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts/lookup" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"users": []map[string]any{{"localId": "u1", "email": "test@codeq.local", "role": "USER"}},
		})
	}))
	t.Cleanup(identitySrv.Close)

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
		Port:                            0,
		RedisAddr:                       mr.Addr(),
		IdentityServiceURL:              identitySrv.URL,
		IdentityServiceApiKey:           "test-key",
		Timezone:                        "UTC",
		LogLevel:                        "error",
		LogFormat:                       "json",
		Env:                             "test",
		DefaultLeaseSeconds:             60,
		RequeueInspectLimit:             50,
		LocalArtifactsDir:               "/tmp/codeq-artifacts-test",
		MaxAttemptsDefault:              5,
		BackoffPolicy:                   "fixed",
		BackoffBaseSeconds:              1,
		BackoffMaxSeconds:               3,
		WorkerJwksURL:                   jwksSrv.URL,
		WorkerAudience:                  "codeq-worker",
		WorkerIssuer:                    "codeq-test",
		AllowedClockSkewSeconds:         60,
		WebhookHmacSecret:               "secret",
		SubscriptionMinIntervalSeconds:  5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:        3,
		ResultWebhookBaseBackoffSeconds: 1,
		ResultWebhookMaxBackoffSeconds:  2,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validate: %v", err)
	}

	app := NewApplication(cfg)
	SetupMappings(app)
	server := httptest.NewServer(app.Engine)
	t.Cleanup(server.Close)

	workerToken := signWorkerJWT(t, privKey, kid, "codeq-test", "codeq-worker", "worker-1")
	producerToken := "producer-token"

	taskID := createTask(t, ctx, server.URL, producerToken, hookSrv.URL)
	claimTask(t, ctx, server.URL, workerToken, taskID)
	heartbeatTask(t, ctx, server.URL, workerToken, taskID)
	nackTask(t, ctx, server.URL, workerToken, taskID)
	claimTask(t, ctx, server.URL, workerToken, taskID)
	submitResult(t, ctx, server.URL, workerToken, taskID)
	getResult(t, ctx, server.URL, producerToken, taskID)

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
