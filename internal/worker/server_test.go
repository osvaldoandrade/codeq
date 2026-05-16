package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/app"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/internal/worker/workerpb"
)

// freePort returns an OS-assigned ephemeral port. Tiny race window
// between Close and the gRPC server's Bind, but it's the same pattern
// the cluster tests use.
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

// TestWorkerStream_HelloAndClaim verifies the basic round-trip:
//   - Hello → HelloAck
//   - Producer enqueues a task via HTTP
//   - Worker sends Ready → server pushes Task
//   - Worker sends Result → server replies ResultAck(ok=true)
func TestWorkerStream_HelloAndClaim(t *testing.T) {
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
		t.Fatalf("validate: %v", err)
	}
	a, err := app.NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.TracingShutdown(ctx)
	}()
	app.SetupMappings(a)

	httpSrv := httptest.NewServer(a.Engine)
	defer httpSrv.Close()

	// Allow the gRPC server a beat to bind. NewApplication returns before
	// Serve has accepted, in practice.
	time.Sleep(150 * time.Millisecond)

	// Producer side: classic HTTP path, doesn't change.
	body := `{"command":"GENERATE_MASTER","payload":{"k":"v"},"priority":5}`
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/codeq/tasks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /tasks: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create returned %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Worker side: open gRPC stream.
	conn, err := grpc.Dial(streamAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial worker gRPC: %v", err)
	}
	defer conn.Close()
	client := workerpb.NewWorkerStreamClient(conn)
	stream, err := client.Stream(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Hello first.
	if err := stream.Send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Hello{
		Hello: &workerpb.Hello{Token: "dev-token"},
	}}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	helloAck, ok := ack.Event.(*workerpb.ServerEvent_HelloAck)
	if !ok {
		t.Fatalf("first response was %T, want HelloAck", ack.Event)
	}
	if helloAck.HelloAck.WorkerId == "" {
		t.Fatal("HelloAck missing worker id")
	}
	if helloAck.HelloAck.TenantId != "dev-tenant" {
		t.Fatalf("HelloAck tenant=%q, want dev-tenant", helloAck.HelloAck.TenantId)
	}

	// Ready → expect a Task assignment.
	if err := stream.Send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Ready{
		Ready: &workerpb.Ready{Commands: []string{"GENERATE_MASTER"}, LeaseSeconds: 60},
	}}); err != nil {
		t.Fatalf("send ready: %v", err)
	}
	got, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv task: %v", err)
	}
	taskEv, ok := got.Event.(*workerpb.ServerEvent_Task)
	if !ok {
		t.Fatalf("got %T, want Task; got=%+v", got.Event, got)
	}
	if taskEv.Task.Task.Id == "" {
		t.Fatal("Task assignment missing id")
	}

	// Result → expect ResultAck(ok=true).
	if err := stream.Send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Result{
		Result: &workerpb.Result{
			TaskId:     taskEv.Task.Task.Id,
			Status:     "COMPLETED",
			ResultJson: []byte(`{"ok":true}`),
		},
	}}); err != nil {
		t.Fatalf("send result: %v", err)
	}
	got, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv result ack: %v", err)
	}
	resAck, ok := got.Event.(*workerpb.ServerEvent_ResultAck)
	if !ok {
		t.Fatalf("got %T, want ResultAck", got.Event)
	}
	if !resAck.ResultAck.Ok {
		t.Fatalf("ResultAck not ok: %s", resAck.ResultAck.ErrorMessage)
	}
	if resAck.ResultAck.TaskId != taskEv.Task.Task.Id {
		t.Fatalf("ResultAck task id mismatch: got %s, want %s", resAck.ResultAck.TaskId, taskEv.Task.Task.Id)
	}
}
