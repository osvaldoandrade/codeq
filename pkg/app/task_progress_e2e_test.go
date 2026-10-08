package app

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	progressCommand = "progress-export"
	keyProgress     = "progress"
)

// TestTaskProgressE2E drives worker progress through the production router:
// the lease holder writes it, readers of the task see it, it survives a
// nack and re-claim, and every refusal maps to its documented status.
func TestTaskProgressE2E(t *testing.T) {
	s := newBindingSuite(t)
	t.Run("static worker", func(t *testing.T) { s.t = t; s.progressLifecycle() })
	t.Run("binding worker", func(t *testing.T) { s.t = t; s.progressBindingOwnership() })
}

func (s *bindingSuite) postProgress(id, token string, body any) apiResult {
	s.t.Helper()
	return s.call(http.MethodPost, pathTasks+"/"+id+"/"+keyProgress, token, body)
}

func (s *bindingSuite) expectProgress(id, token string, want any, label string) {
	s.t.Helper()
	r := s.call(http.MethodGet, pathTasks+"/"+id, token, nil)
	s.expect(r, http.StatusOK, "", label)
	if !reflect.DeepEqual(r.body[keyProgress], want) {
		s.t.Fatalf("%s: progress %v, want %v (body %s)", label, r.body[keyProgress], want, r.raw)
	}
}

func (s *bindingSuite) claimProgressTask() string {
	s.t.Helper()
	r := s.call(http.MethodPost, pathClaim, staticWorkerTok, map[string]any{keyCommands: []string{progressCommand}})
	s.expect(r, http.StatusOK, "", "static worker claim")
	id, _ := r.body["id"].(string)
	return id
}

func (s *bindingSuite) progressLifecycle() {
	id := s.createTask(staticProducerTok, progressCommand)
	s.expectProgress(id, staticProducerTok, nil, "no progress before the first report")
	if got := s.claimProgressTask(); got != id {
		s.t.Fatalf("claimed %s, want %s", got, id)
	}
	report := map[string]any{"processed": float64(250), "total": float64(1000)}
	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{keyProgress: report}), http.StatusOK, "", "owner progress")
	s.expectProgress(id, staticProducerTok, report, "producer reads progress")
	s.expectProgress(id, staticWorkerTok, report, "worker reads progress")

	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{keyProgress: 1, "extra": true}), http.StatusBadRequest, "", "unknown field")
	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{}), http.StatusBadRequest, "progress is required", "missing progress")
	tooLarge := map[string]any{keyProgress: strings.Repeat("x", 64<<10)}
	s.expect(s.postProgress(id, staticWorkerTok, tooLarge), http.StatusRequestEntityTooLarge, "progress too large", "oversized progress")
	s.expect(s.postProgress("does-not-exist", staticWorkerTok, map[string]any{keyProgress: 1}), http.StatusNotFound, "not-found", "unknown task")
	s.expectProgress(id, staticProducerTok, report, "refused reports leave progress unchanged")

	s.expect(s.call(http.MethodPost, pathTasks+"/"+id+"/nack", staticWorkerTok, map[string]any{fwdDelay: 0}), http.StatusOK, "", "nack")
	s.expectProgress(id, staticProducerTok, report, "progress kept after nack")
	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{keyProgress: 1}), http.StatusForbidden, "not-owner", "progress on a requeued task")
	if got := s.claimProgressTask(); got != id {
		s.t.Fatalf("re-claimed %s, want %s", got, id)
	}
	s.expectProgress(id, staticWorkerTok, report, "progress visible after re-claim")
	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{keyProgress: "done"}), http.StatusOK, "", "progress after re-claim")
	s.expect(s.call(http.MethodPost, pathTasks+"/"+id+"/result", staticWorkerTok, map[string]any{keyStatus: statusCompleted, keyResult: okResult}), http.StatusOK, "", "result")
	// Completion releases the lease, so the former holder is no longer the owner.
	s.expect(s.postProgress(id, staticWorkerTok, map[string]any{keyProgress: 2}), http.StatusForbidden, "not-owner", "progress after completion")
	s.expectProgress(id, staticProducerTok, "done", "progress kept after completion")
}

func (s *bindingSuite) progressBindingOwnership() {
	publish := s.bindingToken(policyPublish, tenantConveste, bindingTopic, "pod-1", nil)
	subscribe := s.bindingToken(policySubscribe, tenantConveste, bindingTopic, "pod-1", nil)
	otherPod := s.bindingToken(policySubscribe, tenantConveste, bindingTopic, "pod-2", nil)
	s.drain(subscribe)
	id := s.createTask(publish, bindingTopic)
	s.expect(s.claim(subscribe), http.StatusOK, "", "binding claim")
	body := map[string]any{keyProgress: map[string]any{"pct": float64(10)}}
	s.expect(s.postProgress(id, otherPod, body), http.StatusForbidden, "not-owner", "other pod")
	s.expect(s.postProgress(id, publish, body), http.StatusUnauthorized, "", "publish token")
	s.expect(s.postProgress(id, subscribe, body), http.StatusOK, "", "binding owner")
	s.expectProgress(id, publish, body[keyProgress], "publisher reads progress")
}
