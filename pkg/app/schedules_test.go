package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/config"
)

const (
	keyCron          = "cron"
	scheduleCommand  = "SCHEDULED_SYNC"
	scheduleEndpoint = "/v1/codeq/admin/schedules/every-second"
)

type scheduleView struct {
	Version    int64     `json:"version"`
	NextRunAt  time.Time `json:"nextRunAt"`
	LastTaskID string    `json:"lastTaskId"`
}

func readyTasks(t *testing.T, url string) int {
	t.Helper()
	var stats struct{ Ready int }
	status, raw := doJSON(t, context.Background(), http.MethodGet, url+"/v1/codeq/admin/queues/"+scheduleCommand, "dev-token", nil, &stats)
	if status != http.StatusOK {
		t.Fatalf("queue stats: %d %s", status, raw)
	}
	return stats.Ready
}

func waitReady(t *testing.T, url string, atLeast int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := readyTasks(t, url); n >= atLeast {
			return n
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fewer than %d scheduled tasks after %s (have %d)", atLeast, timeout, readyTasks(t, url))
	return 0
}

func TestSchedulesFireOverHTTP(t *testing.T) {
	a, err := NewApplication(newTopicPebbleConfig(t, t.TempDir()+"/pebble"))
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() {
		srv.Close()
		_ = a.TracingShutdown(context.Background())
	})
	ctx := context.Background()
	spec := map[string]any{keyCron: "@every 1s", keyCommand: scheduleCommand, keyPayload: map[string]bool{"full": true}}

	var created scheduleView
	if status, raw := doJSON(t, ctx, http.MethodPut, srv.URL+scheduleEndpoint, "dev-token", spec, &created); status != http.StatusCreated || created.Version != 1 {
		t.Fatalf("create: %d %s", status, raw)
	}
	if status, _ := doJSON(t, ctx, http.MethodPut, srv.URL+scheduleEndpoint, "dev-token", spec, nil); status != http.StatusOK {
		t.Fatalf("identical replay: status %d, want 200", status)
	}
	bad := map[string]any{keyCron: "61 * * * *", keyCommand: scheduleCommand}
	if status, _ := doJSON(t, ctx, http.MethodPut, srv.URL+scheduleEndpoint, "dev-token", bad, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid cron: status %d, want 422", status)
	}

	waitReady(t, srv.URL, 2, 10*time.Second)
	var got scheduleView
	if status, raw := doJSON(t, ctx, http.MethodGet, srv.URL+scheduleEndpoint, "dev-token", nil, &got); status != http.StatusOK || got.LastTaskID == "" {
		t.Fatalf("get after firing: %d %s", status, raw)
	}
	var list struct {
		Schedules []scheduleView `json:"schedules"`
	}
	if status, raw := doJSON(t, ctx, http.MethodGet, srv.URL+"/v1/codeq/admin/schedules", "dev-token", nil, &list); status != http.StatusOK || len(list.Schedules) != 1 {
		t.Fatalf("list: %d %s", status, raw)
	}

	if status, _ := doJSON(t, ctx, http.MethodDelete, srv.URL+scheduleEndpoint, "dev-token", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: status %d", status)
	}
	time.Sleep(1500 * time.Millisecond) // let a tick in flight finish
	stopped := readyTasks(t, srv.URL)
	time.Sleep(2500 * time.Millisecond)
	if after := readyTasks(t, srv.URL); after != stopped {
		t.Fatalf("a deleted schedule kept firing: %d → %d tasks", stopped, after)
	}
}

func TestSchedulesRequireAdmin(t *testing.T) {
	cfg := newTopicPebbleConfig(t, t.TempDir()+"/pebble")
	cfg.ProducerAuthConfig = json.RawMessage(`{"token":"dev-token","subject":"producer-dev","raw":{"role":"USER","tenantId":"dev-tenant"}}`)
	a, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() {
		srv.Close()
		_ = a.TracingShutdown(context.Background())
	})
	status, _ := doJSON(t, context.Background(), http.MethodPut, srv.URL+scheduleEndpoint, "dev-token", map[string]any{keyCron: "@hourly", keyCommand: scheduleCommand}, nil)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("non-admin PUT: status %d, want 401 or 403", status)
	}
}

func TestRaftSchedulesRequireExplicitProtocol(t *testing.T) {
	ports := pickThreeFreePorts(t)
	cfg := newTopicPebbleConfig(t, t.TempDir()+"/pebble")
	cfg.Raft = config.RaftConfig{
		Enabled: true, SelfID: topicNodeOne, BindAddr: "127.0.0.1:" + ports[0], Bootstrap: true,
		Peers:       map[string]string{topicNodeOne: "127.0.0.1:" + ports[0]},
		HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10, ApplyTimeoutSeconds: 3,
	}
	a, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() {
		srv.Close()
		_ = a.TracingShutdown(context.Background())
	})
	status, raw := doJSON(t, context.Background(), http.MethodPut, srv.URL+scheduleEndpoint, "dev-token", map[string]any{keyCron: "@hourly", keyCommand: scheduleCommand}, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("raft without scheduleCatalogProtocol: %d %s, want 503", status, raw)
	}
}

// TestRaftSchedulesSurviveLeaderTransfer creates a one-second schedule on a
// three-voter cluster, stops the leader, and checks that the new leader
// keeps firing from the replicated catalog. No more tasks than elapsed
// slots exist at the end: a slot is never enqueued twice.
func TestRaftSchedulesSurviveLeaderTransfer(t *testing.T) {
	ports := pickThreeFreePorts(t)
	peers := map[string]string{
		topicNodeOne:   "127.0.0.1:" + ports[0],
		topicNodeTwo:   "127.0.0.1:" + ports[1],
		topicNodeThree: "127.0.0.1:" + ports[2],
	}
	nodes := make([]*raftTestNode, 3)
	for i, id := range []string{topicNodeOne, topicNodeTwo, topicNodeThree} {
		nodes[i] = startRaftNode(t, id, peers, i == 0)
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			if node != nil && !node.closed.Load() {
				_ = node.shutdown()
			}
		}
	})
	ctx := context.Background()
	leader, _ := waitForLeader(t, nodes, 5*time.Second)
	start := time.Now()
	var created scheduleView
	if status, raw := doJSON(t, ctx, http.MethodPut, leader.server.URL+scheduleEndpoint, "dev-token", map[string]any{keyCron: "@every 1s", keyCommand: scheduleCommand}, &created); status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	before := waitReady(t, leader.server.URL, 2, 10*time.Second)

	if err := leader.shutdown(); err != nil {
		t.Fatalf("leader shutdown: %v", err)
	}
	survivors := make([]*raftTestNode, 0, 2)
	for _, node := range nodes {
		if node != leader {
			survivors = append(survivors, node)
		}
	}
	newLeader, _ := waitForLeader(t, survivors, 5*time.Second)
	after := waitReady(t, newLeader.server.URL, before+2, 15*time.Second)

	var final scheduleView
	if status, raw := doJSON(t, ctx, http.MethodGet, newLeader.server.URL+scheduleEndpoint, "dev-token", nil, &final); status != http.StatusOK || final.LastTaskID == "" {
		t.Fatalf("get on new leader: %d %s", status, raw)
	}
	slots := int(time.Since(start)/time.Second) + 1
	if after > slots {
		t.Fatalf("%d tasks for at most %d one-second slots: a slot was enqueued twice", after, slots)
	}
}
