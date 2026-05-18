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

// TestRaft_MultiShard_1Node boots one codeq with numShards=4 +
// raft.enabled=true. Each shard bootstraps its own 1-voter raft group
// on BindAddr+shardIdx. Tasks distribute across shards via ShardedTask-
// Repository's fnv64a routing; each shard's writes replicate through
// its own raft group.
//
// This is the M2.T4 smoke: verify the multi-shard raft topology comes
// up cleanly and the standard create → claim → submit cycle works.
func TestRaft_MultiShard_1Node(t *testing.T) {
	numShards := 4
	basePort := pickContiguousFreePorts(t, numShards)
	pebbleDir := t.TempDir() + "/pebble"
	pcfg, _ := json.Marshal(map[string]any{
		"path":      pebbleDir,
		"numShards": numShards,
	})

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
		RedisAddr:                          "127.0.0.1:0",
		Raft: config.RaftConfig{
			Enabled:             true,
			SelfID:              "node-1",
			BindAddr:            fmt.Sprintf("127.0.0.1:%d", basePort),
			Bootstrap:           true,
			HeartbeatMS:         50,
			ElectionMS:          50,
			LeaderLeaseMS:       50,
			CommitMS:            10,
			ApplyTimeoutSeconds: 3,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	defer func() {
		if app.TracingShutdown != nil {
			_ = app.TracingShutdown(context.Background())
		}
	}()
	SetupMappings(app)

	srv := httptest.NewServer(app.Engine)
	defer srv.Close()

	// Wait for all shards to elect (single-voter, sub-100ms each).
	if !waitWritesAccepted(t, srv, 3*time.Second) {
		t.Fatal("never reached a state where writes succeed across all 4 shards")
	}

	// Submit 40 tasks. With fnv64a routing on uuid task IDs, the
	// distribution should be roughly even across 4 shards. Retry on
	// transient "not leader" — shards elect independently within a few
	// election timeouts, so the first writes may land on a still-
	// settling shard until every group has a leader. This matches the
	// expected client behavior.
	ids := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		body := fmt.Sprintf(`{"command":"GENERATE_MASTER","payload":{"i":%d},"priority":5}`, i)
		id, err := createTaskWithRetry(t, srv.URL, body, 3*time.Second)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Each task readable via GET — confirms the local read path works
	// after multi-shard raft writes.
	for _, id := range ids {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/codeq/tasks/"+id, nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", id, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status %d", id, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Claim everything (round-robin across shards inside ShardedTask-
	// Repository) and submit results. Each cycle exercises that
	// shard's raft pipeline.
	for range ids {
		claimReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/claim",
			strings.NewReader(`{"commands":["GENERATE_MASTER"],"leaseSeconds":60,"waitSeconds":0}`))
		claimReq.Header.Set("Authorization", "Bearer dev-token")
		claimReq.Header.Set("Content-Type", "application/json")
		claimResp, err := http.DefaultClient.Do(claimReq)
		if err != nil || claimResp.StatusCode != http.StatusOK {
			if claimResp != nil {
				claimResp.Body.Close()
			}
			t.Fatalf("claim: err=%v status=%d", err, statusOf(claimResp))
		}
		var out map[string]any
		_ = json.NewDecoder(claimResp.Body).Decode(&out)
		claimResp.Body.Close()
		claimedID, _ := out["id"].(string)
		if claimedID == "" {
			t.Fatalf("claim returned no id (queue empty before draining all 40 tasks)")
		}

		submitReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks/"+claimedID+"/result",
			strings.NewReader(`{"status":"COMPLETED","result":{"ok":true}}`))
		submitReq.Header.Set("Authorization", "Bearer dev-token")
		submitReq.Header.Set("Content-Type", "application/json")
		submitResp, err := http.DefaultClient.Do(submitReq)
		if err != nil || submitResp.StatusCode != http.StatusOK {
			if submitResp != nil {
				submitResp.Body.Close()
			}
			t.Fatalf("submit %s: err=%v status=%d", claimedID, err, statusOf(submitResp))
		}
		submitResp.Body.Close()
	}
}

func statusOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// createTaskWithRetry posts a create and retries on transient "not
// leader" errors. Each task ID is fresh per attempt — fnv64a hash on
// a new UUID reshuffles which shard the write targets, so retrying
// after the slow-elector finishes electing succeeds quickly.
func createTaskWithRetry(t *testing.T, baseURL, body string, timeout time.Duration) (string, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/codeq/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(30 * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusAccepted {
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			id, _ := out["id"].(string)
			return id, nil
		}
		// Capture the body for diagnostics; on "not leader" retry.
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		resp.Body.Close()
		lastBody = string(b[:n])
		if !strings.Contains(lastBody, "not leader") {
			return "", fmt.Errorf("status %d body=%s", resp.StatusCode, lastBody)
		}
		time.Sleep(30 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for leader (last=%s)", lastBody)
}

// waitWritesAccepted polls POST /v1/codeq/tasks until a write succeeds.
// With multi-shard raft, every shard's leader has to be elected before
// we can guarantee every routed write lands; the helper waits until at
// least one write returns 202 (sentinel that the cluster has settled
// for the shard that uuid happens to hash to).
func waitWritesAccepted(t *testing.T, srv *httptest.Server, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	body := `{"command":"GENERATE_MASTER","payload":{"probe":true},"priority":5}`
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		req = req.WithContext(ctx)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil && resp != nil && resp.StatusCode == http.StatusAccepted {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

// pickContiguousFreePorts returns a base port such that base+0..base+n-1
// are all currently free on 127.0.0.1. Strategy: probe upward from a
// random high port, opening a listener at each candidate to confirm
// availability, then close them.
//
// There's a small race between this function returning and the test
// binding the same ports, but on a single-machine test run with no
// other listeners in that range, it's effectively zero.
func pickContiguousFreePorts(t *testing.T, n int) int {
	t.Helper()
	const startRange = 25000
	const endRange = 30000
	for attempt := 0; attempt < 32; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("seed listener: %v", err)
		}
		_, portStr, _ := net.SplitHostPort(l.Addr().String())
		_ = l.Close()
		var base int
		fmt.Sscanf(portStr, "%d", &base)
		if base < startRange || base > endRange-n {
			// Re-roll into the high range
			base = startRange + (base % (endRange - startRange))
		}
		listeners := make([]net.Listener, 0, n)
		ok := true
		for i := 0; i < n; i++ {
			ll, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			listeners = append(listeners, ll)
		}
		for _, ll := range listeners {
			_ = ll.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("could not find %d contiguous free ports after 32 attempts", n)
	return 0
}
