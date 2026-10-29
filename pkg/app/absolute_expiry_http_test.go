// Package app wires the HTTP application and its persistence adapters.
package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	logsDLQKey        = "deadLetterTopicRef"
	logsRetentionKey  = "retentionSeconds"
	logsPrioritiesKey = "priorityTiers"
	logsAttemptsKey   = "maxAttempts"
	logsCreatedKey    = "createdAt"

	logsTestTopic    = "cf-logs"
	logsTestDLQ      = "cf-logs-dlq"
	logsAbsoluteFlag = "absoluteExpiry"
	logsBatchKey     = "batch"
	logsExpiryKey    = "expiresAt"
)

func TestAbsoluteExpiryHTTPRequiresDeadlineAndKeepsOriginalReceipt(t *testing.T) {
	srv := newAbsoluteExpiryServer(t, 0)
	ctx := context.Background()
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	status, raw := doJSON(t, ctx, http.MethodPut, srv.URL+"/v1/codeq/admin/topics/cf-logs", "dev-token", policy, nil)
	if status != http.StatusCreated {
		t.Fatalf("absolute topic: %d %s", status, raw)
	}
	body := map[string]any{keyCommand: logsTestTopic, keyPayload: map[string]any{logsBatchKey: "one"}, fwdTaskID: "logs-batch-one", logsAttemptsKey: 100}
	if status, raw = doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != http.StatusBadRequest {
		t.Fatalf("missing expiry: %d %s", status, raw)
	}
	body[logsExpiryKey] = time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	var first map[string]any
	if status, raw = doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, &first); status != http.StatusAccepted {
		t.Fatalf("create: %d %s", status, raw)
	}
	if first[logsExpiryKey] != body[logsExpiryKey] {
		t.Fatalf("expiry = %v, want %v", first[logsExpiryKey], body[logsExpiryKey])
	}
	body[logsExpiryKey] = time.Now().Add(35 * time.Minute).UTC().Format(time.RFC3339Nano)
	var replay map[string]any
	if status, raw = doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, &replay); status != http.StatusAccepted {
		t.Fatalf("replay: %d %s", status, raw)
	}
	if replay[logsExpiryKey] != first[logsExpiryKey] || replay[logsCreatedKey] != first[logsCreatedKey] {
		t.Fatalf("replay renewed metadata: first=%v replay=%v", first, replay)
	}
	body[keyPayload] = map[string]any{logsBatchKey: "changed"}
	if status, raw = doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != http.StatusConflict {
		t.Fatalf("changed content: %d %s", status, raw)
	}
}

func TestAbsoluteExpiryHTTPRefusesNonDurableActivation(t *testing.T) {
	srv := newListTasksServer(t, "ADMIN")
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	if status, raw := doJSON(t, context.Background(), http.MethodPut, srv.URL+"/v1/codeq/admin/topics/cf-logs", "dev-token", policy, nil); status != http.StatusUnprocessableEntity || !strings.Contains(raw, "fsyncOnCommit=true") {
		t.Fatalf("non-durable activation=%d %s", status, raw)
	}
}

func TestAbsoluteExpiryHTTPMalformedAndExpiredDeadlines(t *testing.T) {
	srv := newAbsoluteExpiryServer(t, 0)
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	if status, raw := doJSON(t, context.Background(), http.MethodPut, srv.URL+"/v1/codeq/admin/topics/cf-logs", "dev-token", policy, nil); status != http.StatusCreated {
		t.Fatalf("topic=%d %s", status, raw)
	}
	for _, tc := range []struct {
		value  any
		status int
	}{{"bad", 400}, {123, 400}, {"0001-01-01T00:00:00Z", 400}, {time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), 410}, {time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano), 400}} {
		body := map[string]any{keyCommand: logsTestTopic, fwdTaskID: "logs-invalid", keyPayload: map[string]int{logsBatchKey: 1}, logsExpiryKey: tc.value}
		if status, raw := doJSON(t, context.Background(), http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != tc.status {
			t.Errorf("expiry=%v status=%d %s want%d", tc.value, status, raw, tc.status)
		}
	}
}

func TestAbsoluteExpiryHTTPBudgetReturnsActionable429AndPreservesLegacy(t *testing.T) {
	srv := newAbsoluteExpiryServer(t, 1)
	ctx := context.Background()
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	if status, raw := doJSON(t, ctx, http.MethodPut, srv.URL+"/v1/codeq/admin/topics/cf-logs", "dev-token", policy, nil); status != 201 {
		t.Fatalf("topic=%d %s", status, raw)
	}
	body := map[string]any{keyCommand: logsTestTopic, fwdTaskID: "logs-budget-one", keyPayload: map[string]int{logsBatchKey: 1}, logsExpiryKey: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != 202 {
		t.Fatalf("first=%d %s", status, raw)
	}
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != 202 {
		t.Fatalf("replay at capacity=%d %s", status, raw)
	}
	body[fwdTaskID] = "logs-budget-two"
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/codeq/tasks", strings.NewReader(string(rawBody)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer dev-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "30" {
		t.Fatalf("budget status=%d retry=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", map[string]any{keyCommand: "ordinary", keyPayload: map[string]int{"keep": 1}}, nil); status != 202 {
		t.Fatalf("legacy affected=%d %s", status, raw)
	}
}

func TestAbsoluteExpiryRaftThreeNodeFailoverKeepsReceipt(t *testing.T) {
	ports := pickThreeFreePorts(t)
	peers := map[string]string{topicNodeOne: "127.0.0.1:" + ports[0], topicNodeTwo: "127.0.0.1:" + ports[1], topicNodeThree: "127.0.0.1:" + ports[2]}
	nodes := make([]*raftTestNode, 0, 3)
	for i, id := range []string{topicNodeOne, topicNodeTwo, topicNodeThree} {
		nodes = append(nodes, startRaftNode(t, id, peers, i == 0, true))
	}
	t.Cleanup(func() {
		for _, node := range nodes {
			_ = node.shutdown()
		}
	})
	leader, _ := waitForLeader(t, nodes, 5*time.Second)
	ctx := context.Background()
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	if status, raw := doJSON(t, ctx, http.MethodPut, leader.server.URL+"/v1/codeq/admin/topics/cf-logs", "dev-token", policy, nil); status != http.StatusCreated {
		t.Fatalf("replicated policy=%d %s", status, raw)
	}
	body := map[string]any{keyCommand: logsTestTopic, keyPayload: map[string]int{logsBatchKey: 1}, fwdTaskID: "logs-raft-failover", logsExpiryKey: time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)}
	var first map[string]any
	if status, raw := doJSON(t, ctx, http.MethodPost, leader.server.URL+"/v1/codeq/tasks", "dev-token", body, &first); status != http.StatusAccepted {
		t.Fatalf("replicated create=%d %s", status, raw)
	}
	if err := leader.shutdown(); err != nil {
		t.Fatal(err)
	}
	survivors := make([]*raftTestNode, 0, 2)
	for _, node := range nodes {
		if node != leader {
			survivors = append(survivors, node)
		}
	}
	next, _ := waitForLeader(t, survivors, 5*time.Second)
	body[logsExpiryKey] = time.Now().Add(35 * time.Minute).UTC().Format(time.RFC3339Nano)
	var replay map[string]any
	if status, raw := doJSON(t, ctx, http.MethodPost, next.server.URL+"/v1/codeq/tasks", "dev-token", body, &replay); status != http.StatusAccepted {
		t.Fatalf("post-failover replay=%d %s", status, raw)
	}
	if replay[logsCreatedKey] != first[logsCreatedKey] || replay[logsExpiryKey] != first[logsExpiryKey] {
		t.Fatalf("failover renewed accepted receipt: first=%v replay=%v", first, replay)
	}
}

func TestAbsoluteExpiryStrictBindingJWKSHTTPWorkerLifecycle(t *testing.T) {
	s := newBindingSuite(t, true)
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	s.expect(s.call(http.MethodPut, "/v1/codeq/admin/topics/cf-logs", s.adminToken(tenantConveste), policy), http.StatusCreated, "", "absolute policy")
	carrier := func(raw map[string]any) { raw["codeq_binding"].(map[string]any)["uid"] = "obs-config-uid" }
	publish := s.bindingToken(policyPublish, tenantConveste, logsTestTopic, "collector", carrier)
	worker := s.bindingToken(policySubscribe, tenantConveste, logsTestTopic, "logs-worker", carrier)
	body := map[string]any{keyCommand: logsTestTopic, keyPayload: map[string]any{"batchId": "one"}, fwdTaskID: "logs-strict", logsAttemptsKey: 100, logsExpiryKey: time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)}
	created := s.call(http.MethodPost, pathTasks, publish, body)
	s.expect(created, http.StatusAccepted, "", "strict Publish")
	claimed := s.call(http.MethodPost, pathClaim, worker, map[string]any{"commands": []string{logsTestTopic}, fwdLease: 60})
	s.expect(claimed, http.StatusOK, "", "strict Claim")
	if claimed.body["id"] != "logs-strict" || claimed.body[logsCreatedKey] != created.body[logsCreatedKey] || claimed.body[logsExpiryKey] != created.body[logsExpiryKey] {
		t.Fatalf("strict Claim lost trusted task metadata: %s", claimed.raw)
	}
	s.expect(s.call(http.MethodPost, pathTasks+"/logs-strict/heartbeat", worker, map[string]any{fwdExtend: 60}), http.StatusOK, "", "strict heartbeat")
	read := s.call(http.MethodGet, pathTasks+"/logs-strict", publish, nil)
	s.expect(read, http.StatusOK, "", "strict read")
	if read.body[logsExpiryKey] != created.body[logsExpiryKey] {
		t.Fatal("worker lifecycle extended expiry")
	}
	s.expect(s.call(http.MethodPost, pathTasks+"/logs-strict/result", worker, map[string]any{keyStatus: statusCompleted, keyResult: map[string]any{"accepted": 1}}), http.StatusOK, "", "strict result")
	// ADR 0012: the acknowledged absolute task is gone at once.
	s.expect(s.call(http.MethodGet, pathTasks+"/logs-strict", publish, nil), http.StatusNotFound, "", "acknowledged read")
}

func TestAbsoluteExpiryHTTPKeepsLargeJSONNumbersSingleAndBatch(t *testing.T) {
	s := newBindingSuite(t, true)
	policy := map[string]any{logsPrioritiesKey: []int{0}, logsAttemptsKey: 100, logsDLQKey: logsTestDLQ, logsRetentionKey: 3600, logsAbsoluteFlag: true}
	s.expect(s.call(http.MethodPut, "/v1/codeq/admin/topics/cf-logs", s.adminToken(tenantConveste), policy), http.StatusCreated, "", "absolute policy")
	publish := s.bindingToken(policyPublish, tenantConveste, logsTestTopic, "precision", nil)
	const payload = `{"large":9007199254740993,"nested":[9223372036854775807,0.1234567890123456789]}`
	for _, batch := range []bool{false, true} {
		id := "logs-precision-single"
		path := pathTasks
		if batch {
			id, path = "logs-precision-batch", pathTasks+"/batch"
		}
		item := map[string]any{keyCommand: logsTestTopic, fwdTaskID: id, keyPayload: json.RawMessage(payload), logsExpiryKey: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
		var body any = item
		if batch {
			body = map[string]any{"tasks": []any{item}}
		}
		response := s.call(http.MethodPost, path, publish, body)
		if batch {
			s.expect(response, http.StatusOK, "", "batch precision")
			response.body = response.body["results"].([]any)[0].(map[string]any)["task"].(map[string]any)
		} else {
			s.expect(response, http.StatusAccepted, "", "single precision")
		}
		if response.body[keyPayload] != payload {
			t.Errorf("batch=%v payload precision lost: %v", batch, response.body[keyPayload])
		}
	}
	// Legacy JSON normalization and null rejection remain unchanged.
	legacy := s.call(http.MethodPost, pathTasks, s.adminToken(tenantConveste), map[string]any{keyCommand: "ordinary", keyPayload: json.RawMessage(payload)})
	s.expect(legacy, http.StatusAccepted, "", "legacy precision compatibility")
	if !strings.Contains(legacy.body[keyPayload].(string), "9007199254740992") {
		t.Fatal("legacy payload normalization changed")
	}
	s.expect(s.call(http.MethodPost, pathTasks, publish, map[string]any{keyCommand: logsTestTopic, keyPayload: nil, logsExpiryKey: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}), http.StatusBadRequest, "", "null payload")
}

func newAbsoluteExpiryServer(t *testing.T, budget int) *httptest.Server {
	t.Helper()
	path := t.TempDir() + "/pebble"
	cfg := newTopicPebbleConfig(t, path)
	cfg.PersistenceConfig, _ = json.Marshal(map[string]any{topicPersistencePathKey: path, "fsyncOnCommit": true})
	cfg.AbsoluteIdentityBudget = budget
	cfg.ProducerAuthConfig = json.RawMessage(`{"token":"dev-token","subject":"producer-dev","raw":{"role":"ADMIN","tenantId":"dev-tenant"}}`)
	a, err := NewApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() { srv.Close(); _ = a.TracingShutdown(context.Background()) })
	return srv
}
