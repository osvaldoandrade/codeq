package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

const (
	topicAdminEndpoint       = "/v1/codeq/admin/topics/events"
	topicNodeOne             = "node-1"
	topicNodeTwo             = "node-2"
	topicNodeThree           = "node-3"
	topicPersistencePathKey  = "path"
	topicTestTimezone        = "UTC"
	topicTestLogLevel        = "error"
	topicTestLogFormat       = "json"
	topicTestEnvironment     = "dev"
	topicTestBackoffPolicy   = "fixed"
	topicTestSecret          = "secret"
	topicTestWorkerAudience  = "codeq-worker"
	topicTestAuthProvider    = "static"
	topicTestStorageProvider = "pebble"
	topicTestRedisAddr       = "127.0.0.1:0"
)

func TestPebbleTopicCatalogSurvivesApplicationRestart(t *testing.T) {
	path := t.TempDir() + "/pebble"
	policy := topicTestPolicy(20)

	first, err := NewApplication(newTopicPebbleConfig(t, path))
	if err != nil {
		t.Fatalf("first NewApplication: %v", err)
	}
	created, wasCreated, changed, err := first.Topics.Upsert(context.Background(), "acme", "events", policy)
	if err != nil || !wasCreated || changed || created.Version != 1 {
		t.Fatalf("create = %#v created=%v changed=%v err=%v", created, wasCreated, changed, err)
	}
	if err := first.TracingShutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	second, err := NewApplication(newTopicPebbleConfig(t, path))
	if err != nil {
		t.Fatalf("second NewApplication: %v", err)
	}
	defer func() { _ = second.TracingShutdown(context.Background()) }()
	got, err := second.Topics.Get(context.Background(), "acme", "events")
	if err != nil || got.Version != 1 || got.Policy.MaxConsumers != 20 {
		t.Fatalf("after restart = %#v err=%v", got, err)
	}
}

func TestRaftTopicCatalogRequiresExplicitProtocol(t *testing.T) {
	ports := pickThreeFreePorts(t)
	cfg := newTopicPebbleConfig(t, t.TempDir()+"/pebble")
	cfg.Raft = config.RaftConfig{
		Enabled:             true,
		SelfID:              topicNodeOne,
		BindAddr:            "127.0.0.1:" + ports[0],
		Bootstrap:           true,
		Peers:               map[string]string{topicNodeOne: "127.0.0.1:" + ports[0]},
		HeartbeatMS:         50,
		ElectionMS:          50,
		LeaderLeaseMS:       50,
		CommitMS:            10,
		ApplyTimeoutSeconds: 3,
	}
	app, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	defer func() { _ = app.TracingShutdown(context.Background()) }()
	_, _, _, err = app.Topics.Upsert(context.Background(), "acme", "events", topicTestPolicy(20))
	var unavailable *queuetopic.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

func TestRaftTopicCatalogReplicationAndLeaderTransfer(t *testing.T) {
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

	leader, _ := waitForLeader(t, nodes, 5*time.Second)
	created := putTopic(t, leader, topicTestPolicy(20), http.StatusCreated)
	if created.Version != 1 || created.TopicID != "dev-tenant.events" {
		t.Fatalf("created = %#v", created)
	}
	replayed := putTopic(t, leader, topicTestPolicy(20), http.StatusOK)
	if replayed.Version != 1 {
		t.Fatalf("replay version = %d", replayed.Version)
	}
	updated := putTopic(t, leader, topicTestPolicy(40), http.StatusOK)
	if updated.Version != 2 {
		t.Fatalf("update version = %d", updated.Version)
	}

	for _, node := range nodes {
		waitTopicStatus(t, node, http.StatusOK, 2)
	}
	var follower *raftTestNode
	for _, node := range nodes {
		if node != leader {
			follower = node
			break
		}
	}
	putTopic(t, follower, topicTestPolicy(80), http.StatusServiceUnavailable)

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
	waitTopicStatus(t, newLeader, http.StatusOK, 2)
	postTransfer := putTopic(t, newLeader, topicTestPolicy(60), http.StatusOK)
	if postTransfer.Version != 3 {
		t.Fatalf("post-transfer version = %d", postTransfer.Version)
	}
	for _, node := range survivors {
		waitTopicStatus(t, node, http.StatusOK, 3)
	}

	status, body := doJSON(t, context.Background(), http.MethodDelete, newLeader.server.URL+topicAdminEndpoint+"?deletionPolicy=Delete", "dev-token", nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", status, body)
	}
	for _, node := range survivors {
		waitTopicStatus(t, node, http.StatusNotFound, 0)
	}
}

func newTopicPebbleConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	persistence, _ := json.Marshal(map[string]any{topicPersistencePathKey: path})
	return &config.Config{
		Port:                               0,
		Timezone:                           topicTestTimezone,
		LogLevel:                           topicTestLogLevel,
		LogFormat:                          topicTestLogFormat,
		Env:                                topicTestEnvironment,
		DefaultLeaseSeconds:                60,
		RequeueInspectLimit:                50,
		LocalArtifactsDir:                  t.TempDir(),
		MaxAttemptsDefault:                 5,
		BackoffPolicy:                      topicTestBackoffPolicy,
		BackoffBaseSeconds:                 1,
		BackoffMaxSeconds:                  3,
		WebhookHmacSecret:                  topicTestSecret,
		WorkerAudience:                     topicTestWorkerAudience,
		SubscriptionMinIntervalSeconds:     5,
		SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts:           1,
		ResultWebhookBaseBackoffSeconds:    1,
		ResultWebhookMaxBackoffSeconds:     2,
		ProducerAuthProvider:               topicTestAuthProvider,
		ProducerAuthConfig:                 json.RawMessage(`{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN","tenantId":"dev-tenant"}}`),
		WorkerAuthProvider:                 topicTestAuthProvider,
		WorkerAuthConfig:                   json.RawMessage(`{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"],"raw":{"tenantId":"dev-tenant"}}`),
		PersistenceProvider:                topicTestStorageProvider,
		PersistenceConfig:                  persistence,
		RedisAddr:                          topicTestRedisAddr,
	}
}

func topicTestPolicy(maxConsumers int) queuetopic.Policy {
	return queuetopic.Policy{
		PriorityTiers:      []int{0, 3},
		MaxAttempts:        5,
		DeadLetterTopicRef: "events-dlq",
		RetentionSeconds:   3600,
		MaxConsumers:       maxConsumers,
	}
}

func putTopic(t *testing.T, node *raftTestNode, policy queuetopic.Policy, wantStatus int) queuetopic.Topic {
	t.Helper()
	var topic queuetopic.Topic
	status, body := doJSON(t, context.Background(), http.MethodPut, node.server.URL+topicAdminEndpoint, "dev-token", policy, &topic)
	if status != wantStatus {
		t.Fatalf("PUT topic on %s status=%d want=%d body=%s", node.id, status, wantStatus, body)
	}
	return topic
}

func waitTopicStatus(t *testing.T, node *raftTestNode, wantStatus int, wantVersion int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var topic queuetopic.Topic
		status, _ := doJSON(t, context.Background(), http.MethodGet, node.server.URL+topicAdminEndpoint, "dev-token", nil, &topic)
		if status == wantStatus && (wantVersion == 0 || topic.Version == wantVersion) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("node %s did not reach topic status=%d version=%d", node.id, wantStatus, wantVersion)
}
