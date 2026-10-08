package app

import (
	"net/http"
	"testing"
	"time"
)

const (
	dlqE2ECommand = "dlq-e2e"
	pathAdmin     = "/v1/codeq/admin"
	pathBulkDLQ   = pathAdmin + "/queues/" + dlqE2ECommand + "/dlq/requeue"
	codeNotInDLQ  = "task_not_in_dlq"
)

// TestDLQAdminE2E drives the dead-letter administration routes through the
// production router: an admin requeues, bulk-requeues and deletes; every
// other token kind is refused, and another tenant's admin sees nothing.
func TestDLQAdminE2E(t *testing.T) {
	s := newBindingSuite(t)
	t.Run("lifecycle", func(t *testing.T) { s.t = t; s.dlqLifecycle() })
	t.Run("access", func(t *testing.T) { s.t = t; s.dlqAccess() })
}

func pathAdminTask(id string) string { return pathAdmin + "/tasks/" + id }

// deadLetterE2E creates a one-attempt task as the static producer, claims
// it as the static worker and nacks it into the dead-letter queue.
func (s *bindingSuite) deadLetterE2E() string {
	s.t.Helper()
	r := s.call(http.MethodPost, pathTasks, staticProducerTok, map[string]any{keyCommand: dlqE2ECommand, keyPayload: map[string]any{"k": "v"}, "maxAttempts": 1})
	s.expect(r, http.StatusAccepted, "", "create")
	id, _ := r.body["id"].(string)
	s.claimE2E(id)
	r = s.call(http.MethodPost, pathTasks+"/"+id+"/nack", staticWorkerTok, map[string]any{fwdDelay: 0, "reason": "boom"})
	s.expect(r, http.StatusOK, "", "nack")
	if r.body[keyStatus] != "dlq" {
		s.t.Fatalf("nack did not dead-letter: %s", r.raw)
	}
	return id
}

func (s *bindingSuite) claimE2E(id string) {
	s.t.Helper()
	r := s.call(http.MethodPost, pathClaim, staticWorkerTok, map[string]any{keyCommands: []string{dlqE2ECommand}})
	s.expect(r, http.StatusOK, "", "claim")
	if r.body["id"] != id {
		s.t.Fatalf("claimed %v, want %s", r.body["id"], id)
	}
}

func (s *bindingSuite) expectTaskStatus(id, want string) {
	s.t.Helper()
	r := s.call(http.MethodGet, pathTasks+"/"+id, staticProducerTok, nil)
	s.expect(r, http.StatusOK, "", "read "+id)
	if r.body[keyStatus] != want {
		s.t.Fatalf("task %s: %s, want status %s", id, r.raw, want)
	}
}

func (s *bindingSuite) dlqLifecycle() {
	id := s.deadLetterE2E()
	s.expectTaskStatus(id, "FAILED")

	r := s.call(http.MethodPost, pathAdminTask(id)+"/requeue", staticProducerTok, nil)
	s.expect(r, http.StatusOK, "", "requeue")
	if r.body[keyStatus] != "PENDING" || r.body["attempts"] != nil || r.body["error"] != nil {
		s.t.Fatalf("requeued task %s", r.raw)
	}
	s.expect(s.call(http.MethodPost, pathAdminTask(id)+"/requeue", staticProducerTok, nil), http.StatusConflict, codeNotInDLQ, "requeue twice")

	s.claimE2E(id)
	s.expect(s.call(http.MethodDelete, pathAdminTask(id), staticProducerTok, nil), http.StatusConflict, "task_in_progress", "delete in progress")
	s.expect(s.call(http.MethodPost, pathTasks+"/"+id+"/nack", staticWorkerTok, map[string]any{fwdDelay: 0}), http.StatusOK, "", "nack again")

	s.expect(s.call(http.MethodPost, pathBulkDLQ+"?limit=ten", staticProducerTok, nil), http.StatusBadRequest, "", "bulk bad limit")
	s.expect(s.call(http.MethodPost, pathBulkDLQ+"?limit=5000", staticProducerTok, nil), http.StatusBadRequest, "", "bulk limit too high")
	r = s.call(http.MethodPost, pathBulkDLQ, staticProducerTok, nil)
	s.expect(r, http.StatusOK, "", "bulk requeue")
	if r.raw != `{"requeued":1,"remaining":false}` {
		s.t.Fatalf("bulk requeue answered %s", r.raw)
	}
	s.expectTaskStatus(id, "PENDING")

	s.claimE2E(id)
	s.expect(s.call(http.MethodPost, pathTasks+"/"+id+"/nack", staticWorkerTok, map[string]any{fwdDelay: 0}), http.StatusOK, "", "nack to delete")
	r = s.call(http.MethodDelete, pathAdminTask(id), staticProducerTok, nil)
	if r.status != http.StatusNoContent || r.raw != "" {
		s.t.Fatalf("delete: %d %q, want 204 with no body", r.status, r.raw)
	}
	s.expect(s.call(http.MethodGet, pathTasks+"/"+id, staticProducerTok, nil), http.StatusNotFound, "", "read deleted")
	s.expect(s.call(http.MethodDelete, pathAdminTask(id), staticProducerTok, nil), http.StatusNotFound, "not found", "delete twice")
	s.expect(s.call(http.MethodPost, pathAdminTask(id)+"/requeue", staticProducerTok, nil), http.StatusNotFound, "not found", "requeue deleted")
}

func (s *bindingSuite) dlqAccess() {
	id := s.deadLetterE2E()
	otherAdmin := s.adminToken(tenantConveste)
	s.expect(s.call(http.MethodPost, pathAdminTask(id)+"/requeue", otherAdmin, nil), http.StatusNotFound, "not found", "other tenant requeue")
	s.expect(s.call(http.MethodDelete, pathAdminTask(id), otherAdmin, nil), http.StatusNotFound, "not found", "other tenant delete")
	r := s.call(http.MethodPost, pathBulkDLQ, otherAdmin, nil)
	s.expect(r, http.StatusOK, "", "other tenant bulk")
	if r.raw != `{"requeued":0,"remaining":false}` {
		s.t.Fatalf("other tenant bulk requeue answered %s", r.raw)
	}

	publish := s.bindingToken(policyPublish, tenantConveste, bindingTopic, "pod-1", nil)
	subscribe := s.bindingToken(policySubscribe, tenantConveste, bindingTopic, "pod-1", nil)
	for _, rc := range []struct{ method, path string }{
		{http.MethodPost, pathAdminTask(id) + "/requeue"},
		{http.MethodDelete, pathAdminTask(id)},
		{http.MethodPost, pathBulkDLQ},
	} {
		s.expect(s.call(rc.method, rc.path, staticWorkerTok, nil), http.StatusUnauthorized, "", "worker "+rc.path)
		s.expect(s.call(rc.method, rc.path, publish, nil), http.StatusForbidden, "route_not_allowed", "binding publish "+rc.path)
		s.expect(s.call(rc.method, rc.path, subscribe, nil), http.StatusUnauthorized, "", "binding subscribe "+rc.path)
	}
	s.expectTaskStatus(id, "FAILED")
}

// TestLeaderForward_DLQAdminThroughFollower sends every dead-letter admin
// write to Raft followers: each is forwarded to the leader and replicated,
// never answered with a redirect.
func TestLeaderForward_DLQAdminThroughFollower(t *testing.T) {
	c := startForwardCluster(t, fwdOptions{shards: 1, httpPeers: true, base: staticForwardConfig})
	leader := c.waitLeaderOfAll(10 * time.Second)
	f := c.others(leader)

	deadLetter := func() string {
		r := c.call(f[0], http.MethodPost, pathTasks, fwdToken, map[string]any{keyCommand: fwdCommand, keyPayload: 1, "maxAttempts": 1}, nil)
		c.expect(r, http.StatusAccepted, "", "create via follower")
		id, _ := r.body["id"].(string)
		r = c.claim(f[1], fwdToken, 0)
		c.expect(r, http.StatusOK, "", "claim via follower")
		if r.body["id"] != id {
			t.Fatalf("claimed %v, want %s", r.body["id"], id)
		}
		r = c.call(f[0], http.MethodPost, pathTasks+"/"+id+"/nack", fwdToken, map[string]any{fwdDelay: 0}, nil)
		c.expect(r, http.StatusOK, "", "nack via follower")
		return id
	}
	leaderStatus := func(id string) string {
		r := c.call(leader, http.MethodGet, pathTasks+"/"+id, fwdToken, nil, nil)
		status, _ := r.body[keyStatus].(string)
		return status
	}

	id := deadLetter()
	c.expect(c.call(f[1], http.MethodPost, pathAdminTask(id)+"/requeue", fwdToken, nil, nil), http.StatusOK, "", "requeue via follower")
	if got := leaderStatus(id); got != "PENDING" {
		t.Fatalf("leader sees %q after a forwarded requeue", got)
	}
	r := c.claim(f[0], fwdToken, 0)
	c.expect(r, http.StatusOK, "", "claim the requeued task")
	c.expect(c.call(f[1], http.MethodPost, pathTasks+"/"+id+"/nack", fwdToken, map[string]any{fwdDelay: 0}, nil), http.StatusOK, "", "dead-letter again")

	r = c.call(f[0], http.MethodPost, "/v1/codeq/admin/queues/"+fwdCommand+"/dlq/requeue", fwdToken, nil, nil)
	c.expect(r, http.StatusOK, "", "bulk requeue via follower")
	if r.raw != `{"requeued":1,"remaining":false}` {
		t.Fatalf("bulk requeue via follower answered %s", r.raw)
	}
	c.expect(c.call(f[1], http.MethodDelete, pathAdminTask(id), fwdToken, nil, nil), http.StatusNoContent, "", "delete via follower")
	c.expect(c.call(leader, http.MethodGet, pathTasks+"/"+id, fwdToken, nil, nil), http.StatusNotFound, "", "leader after delete")
}
