package producerclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/app"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/producerclient"
)

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

type fixture struct {
	streamAddr string
	httpURL    string
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
		ProducerStreamAddr:                 streamAddr,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	a, err := app.NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	app.SetupMappings(a)
	httpSrv := httptest.NewServer(a.Engine)
	time.Sleep(150 * time.Millisecond)
	return &fixture{
		streamAddr: streamAddr,
		httpURL:    httpSrv.URL,
		stop: func() {
			httpSrv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = a.TracingShutdown(ctx)
		},
	}
}

func TestProduce_HappyPath(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := producerclient.New(producerclient.Config{
		Addr:  f.streamAddr,
		Token: "dev-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sess, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	if sess.TenantID() != "dev-tenant" {
		t.Errorf("TenantID=%q, want dev-tenant", sess.TenantID())
	}

	id, err := sess.Produce(ctx, producerclient.CreateRequest{
		Command: "GENERATE_MASTER",
		Payload: []byte(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if id == "" {
		t.Fatal("empty task id")
	}

	// Cross-check via REST.
	req, _ := http.NewRequest(http.MethodGet, f.httpURL+"/v1/codeq/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET task status=%d", resp.StatusCode)
	}
}

func TestProduce_PipelinedConcurrent(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := producerclient.New(producerclient.Config{
		Addr:  f.streamAddr,
		Token: "dev-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	const total = 200
	const concurrency = 16
	var (
		ok      atomic.Int64
		ids     sync.Map // id → struct{}
		wg      sync.WaitGroup
		jobs    = make(chan int, total)
		mu      sync.Mutex
		firstEr error
	)
	for i := range total {
		jobs <- i
	}
	close(jobs)
	for range concurrency {
		wg.Go(func() {
			for i := range jobs {
				id, err := sess.Produce(ctx, producerclient.CreateRequest{
					Command: "GENERATE_MASTER",
					Payload: []byte(fmt.Sprintf(`{"i":%d}`, i)),
				})
				if err != nil {
					mu.Lock()
					if firstEr == nil {
						firstEr = err
					}
					mu.Unlock()
					return
				}
				if id == "" {
					mu.Lock()
					if firstEr == nil {
						firstEr = fmt.Errorf("empty id for i=%d", i)
					}
					mu.Unlock()
					return
				}
				if _, dup := ids.LoadOrStore(id, struct{}{}); dup {
					mu.Lock()
					if firstEr == nil {
						firstEr = fmt.Errorf("duplicate id %s", id)
					}
					mu.Unlock()
					return
				}
				ok.Add(1)
			}
		})
	}
	wg.Wait()

	if firstEr != nil {
		t.Fatalf("produce: %v", firstEr)
	}
	if got := ok.Load(); got != total {
		t.Fatalf("ok=%d, want %d", got, total)
	}
}

func TestProduce_RejectsInvalid(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := producerclient.New(producerclient.Config{Addr: f.streamAddr, Token: "dev-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	_, err = sess.Produce(ctx, producerclient.CreateRequest{
		Command: "", // missing
	})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestProduce_HandshakeRejectsBadToken(t *testing.T) {
	f := newFixture(t)
	defer f.stop()

	c, err := producerclient.New(producerclient.Config{Addr: f.streamAddr, Token: "wrong"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Connect(ctx); err == nil {
		t.Fatal("expected handshake error")
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	if _, err := producerclient.New(producerclient.Config{Token: "x"}); err == nil {
		t.Error("missing Addr should error")
	}
	if _, err := producerclient.New(producerclient.Config{Addr: "x"}); err == nil {
		t.Error("missing Token should error")
	}
}
