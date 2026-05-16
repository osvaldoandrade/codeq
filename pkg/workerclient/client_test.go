package workerclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/workerclient"
)

// freePort returns an OS-assigned ephemeral port.
func freePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

// fixture spins up a fresh codeq Application with Pebble + worker
// streaming enabled and returns the HTTP endpoint (for producing) and
// the gRPC stream endpoint (for the client under test).
type fixture struct {
	httpURL    string
	streamAddr string
	stop       func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	streamPort := freePort(t)
	streamAddr := fmt.Sprintf("127.0.0.1:%d", streamPort)

	cfg := &config.Config{
		Port:                               0,
		Timezone:                           "UTC",
		LogLevel:                           "error",
		LogFormat:                          "json",
		Env:                                "dev",
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  t.TempDir(),
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      "fixed",
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WebhookHmacSecret:                  "secret",
		WorkerAudience:                     "codeq-worker",
		SubscriptionMinIntervalSeconds:     5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:           3,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
		ProducerAuthProvider:               "static",
		ProducerAuthConfig:                 json.RawMessage(`{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN","tenantId":"dev-tenant"}}`),
		WorkerAuthProvider:                 "static",
		WorkerAuthConfig:                   json.RawMessage(`{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"],"raw":{"tenantId":"dev-tenant"}}`),
		PersistenceProvider:                "pebble",
		PersistenceConfig:                  json.RawMessage(fmt.Sprintf(`{"path":"%s"}`, t.TempDir())),
		RedisAddr:                          "127.0.0.1:0",
		WorkerStreamAddr:                   streamAddr,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate cfg: %v", err)
	}
	a, err := app.NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	app.SetupMappings(a)

	httpSrv := httptest.NewServer(a.Engine)

	// Give the gRPC listener a beat to bind.
	time.Sleep(150 * time.Millisecond)

	return &fixture{
		httpURL:    httpSrv.URL,
		streamAddr: streamAddr,
		stop: func() {
			httpSrv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = a.TracingShutdown(ctx)
		},
	}
}

func (f *fixture) enqueue(t *testing.T, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.httpURL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /tasks status=%d", resp.StatusCode)
	}
}

func TestClient_RunCompletedHappyPath(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	f.enqueue(t, `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":1}`)

	c, err := workerclient.New(workerclient.Config{
		Addr:         f.streamAddr,
		Token:        "dev-token",
		Commands:     []string{"GENERATE_MASTER"},
		Concurrency:  1,
		LeaseSeconds: 60,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	var got atomic.Pointer[workerclient.Task]
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = c.Run(ctx, func(_ context.Context, task workerclient.Task) workerclient.Result {
			tCopy := task
			got.Store(&tCopy)
			close(done)
			return workerclient.Completed(map[string]any{"ok": true})
		})
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handler never invoked")
	}

	tk := got.Load()
	if tk == nil {
		t.Fatal("handler invoked with nil task")
	}
	if tk.Command != "GENERATE_MASTER" {
		t.Errorf("Command=%q, want GENERATE_MASTER", tk.Command)
	}
	if tk.TenantID != "dev-tenant" {
		t.Errorf("TenantID=%q, want dev-tenant", tk.TenantID)
	}
	if len(tk.Payload) == 0 || !strings.Contains(string(tk.Payload), `"k":"v"`) {
		t.Errorf("Payload=%q, want JSON with k:v", string(tk.Payload))
	}
	if tk.Priority != 1 {
		t.Errorf("Priority=%d, want 1", tk.Priority)
	}
}

func TestClient_RunFailedAndNackAndAbandon(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	// Three tasks, three different dispositions.
	for i := range 3 {
		f.enqueue(t, fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"i":%d}}`, i))
	}

	c, err := workerclient.New(workerclient.Config{
		Addr:         f.streamAddr,
		Token:        "dev-token",
		Commands:     []string{"GENERATE_MASTER"},
		Concurrency:  3,
		LeaseSeconds: 60,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	var (
		mu       sync.Mutex
		seen     []int
		expected = 3
		doneCh   = make(chan struct{})
	)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	go func() {
		_ = c.Run(ctx, func(_ context.Context, task workerclient.Task) workerclient.Result {
			var raw map[string]any
			_ = json.Unmarshal(task.Payload, &raw)
			i, _ := raw["i"].(float64)
			mu.Lock()
			seen = append(seen, int(i))
			if len(seen) == expected {
				select {
				case <-doneCh:
				default:
					close(doneCh)
				}
			}
			mu.Unlock()
			switch int(i) {
			case 0:
				return workerclient.Completed(nil)
			case 1:
				return workerclient.Failed("intentional")
			case 2:
				return workerclient.Nack(1, "retry")
			}
			return workerclient.Abandon()
		})
	}()

	select {
	case <-doneCh:
	case <-ctx.Done():
		t.Fatalf("only %d/%d tasks processed before timeout", len(seen), expected)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != expected {
		t.Fatalf("seen=%v, want %d entries", seen, expected)
	}
}

func TestClient_HandshakeRejectsBadToken(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := workerclient.New(workerclient.Config{
		Addr:  f.streamAddr,
		Token: "definitely-not-the-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = c.Run(ctx, func(_ context.Context, _ workerclient.Task) workerclient.Result {
		t.Fatal("handler should not run on bad token")
		return workerclient.Completed(nil)
	})
	if err == nil {
		t.Fatal("expected handshake error, got nil")
	}
}

func TestClient_NewValidatesRequiredFields(t *testing.T) {
	if _, err := workerclient.New(workerclient.Config{Token: "x"}); err == nil {
		t.Error("missing Addr should error")
	}
	if _, err := workerclient.New(workerclient.Config{Addr: "x"}); err == nil {
		t.Error("missing Token should error")
	}
}

func TestClient_RunRequiresHandler(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := workerclient.New(workerclient.Config{Addr: f.streamAddr, Token: "dev-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Run(context.Background(), nil); err == nil {
		t.Error("nil handler should error")
	}
}
