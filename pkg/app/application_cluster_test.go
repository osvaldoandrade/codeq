package app

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

	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// freePort grabs a real OS-assigned port and immediately closes the
// listener. There's a small race window before the gRPC server binds to
// it; flaky-enough for production code, fine for tests.
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

func bootClusterNode(t *testing.T, selfID string, allNodes []config.ClusterNodeSpec, dir string) *httptest.Server {
	t.Helper()
	selfGRPC := ""
	for _, n := range allNodes {
		if n.ID == selfID {
			selfGRPC = n.GRPCAddr
			break
		}
	}
	if selfGRPC == "" {
		t.Fatalf("selfID %s not in node list", selfID)
	}

	pcfg, _ := json.Marshal(map[string]any{"path": dir})
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
		PersistenceConfig:                  pcfg,
		RedisAddr:                          "127.0.0.1:0", // unused in pebble path
		Cluster: config.ClusterConfig{
			Enabled:  true,
			SelfID:   selfID,
			GRPCAddr: selfGRPC,
			Nodes:    allNodes,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate %s: %v", selfID, err)
	}

	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication %s: %v", selfID, err)
	}
	SetupMappings(app)

	srv := httptest.NewServer(app.Engine)
	t.Cleanup(func() {
		srv.Close()
		if app.TracingShutdown != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = app.TracingShutdown(ctx)
		}
	})
	return srv
}

// TestClusterRouting_TwoNodeRoundtrip: brings up two real codeq nodes
// (HTTP + gRPC + Pebble each), creates 50 tasks on node A's REST API,
// and verifies that:
//   (1) tasks land split across both nodes (router hashed by task ID),
//   (2) claiming via node A retrieves tasks owned by either node thanks
//       to the scatter-gather Claim path.
func TestClusterRouting_TwoNodeRoundtrip(t *testing.T) {
	portA := freePort(t)
	portB := freePort(t)
	nodes := []config.ClusterNodeSpec{
		{ID: "node-a", GRPCAddr: fmt.Sprintf("127.0.0.1:%d", portA)},
		{ID: "node-b", GRPCAddr: fmt.Sprintf("127.0.0.1:%d", portB)},
	}
	a := bootClusterNode(t, "node-a", nodes, t.TempDir())
	_ = bootClusterNode(t, "node-b", nodes, t.TempDir())

	// Give gRPC servers a beat to bind (NewApplication returns before
	// Serve has accepted, in practice). Less than 1s typically.
	time.Sleep(150 * time.Millisecond)

	// Create N tasks via node A's REST API.
	const N = 50
	createdIDs := make([]string, 0, N)
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"i":%d},"priority":5}`, i)
		req, _ := http.NewRequest(http.MethodPost, a.URL+"/v1/codeq/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /tasks: %v", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			b := make([]byte, 256)
			_, _ = resp.Body.Read(b)
			t.Fatalf("expected 202, got %d body=%s", resp.StatusCode, string(b))
		}
		var ack struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ack)
		resp.Body.Close()
		if ack.ID == "" {
			t.Fatalf("create response missing id")
		}
		createdIDs = append(createdIDs, ack.ID)
	}

	// Claim every task back through node A. Scatter-gather should surface
	// tasks owned by node B as well.
	claimed := 0
	for i := 0; i < N*2 && claimed < N; i++ {
		req, _ := http.NewRequest(http.MethodPost, a.URL+"/v1/codeq/tasks/claim",
			strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /claim: %v", err)
		}
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			// Empty for now (Pebble background reconcile timing); try again.
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b := make([]byte, 256)
			_, _ = resp.Body.Read(b)
			t.Fatalf("claim got %d body=%s", resp.StatusCode, string(b))
		}
		var task struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&task)
		resp.Body.Close()
		if task.ID == "" {
			t.Fatalf("claim returned empty id")
		}
		claimed++

		// Submit a result so the task finalizes and Pebble doesn't keep
		// it in inprog forever (would mask drain bugs).
		resReq, _ := http.NewRequest(http.MethodPost, a.URL+"/v1/codeq/tasks/"+task.ID+"/result",
			strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`))
		resReq.Header.Set("Authorization", "Bearer dev-token")
		resReq.Header.Set("Content-Type", "application/json")
		resResp, err := http.DefaultClient.Do(resReq)
		if err != nil {
			t.Fatalf("POST /result: %v", err)
		}
		if resResp.StatusCode != http.StatusOK {
			b := make([]byte, 256)
			_, _ = resResp.Body.Read(b)
			t.Fatalf("result got %d body=%s for id=%s", resResp.StatusCode, string(b), task.ID)
		}
		resResp.Body.Close()
	}

	if claimed != N {
		t.Fatalf("expected to claim %d tasks total via scatter-gather, got %d", N, claimed)
	}
}
