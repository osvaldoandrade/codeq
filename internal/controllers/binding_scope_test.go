package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	bindingTenant    = "conveste"
	otherTenant      = "other"
	keyCommand       = "command"
	keyTasks         = "tasks"
	taskNotFoundBody = `{"error":"task not found"}`
)

const (
	topicA     = "topic-a"
	keyPayload = "payload"
	keyResult  = "result"
)

const (
	topicB     = "topic-b"
	keyResults = "results"
)

const (
	keyStatus = "status"
	keyTaskID = "taskId"
)

const testSubject = authclaims.BindingSubjectPrefix + "k3s:ns:sa:pod"

func publishScope() *authclaims.BindingScope {
	return &authclaims.BindingScope{Policy: authclaims.PolicyPublish, TenantID: bindingTenant, EventType: topicA, TopicID: "conveste.topic-a", Subject: testSubject}
}

func subscribeScope() *authclaims.BindingScope {
	scope := publishScope()
	scope.Policy = authclaims.PolicySubscribe
	return scope
}

// withScope places a verified scope in the context the way the middleware does.
func withScope(c *gin.Context, scope *authclaims.BindingScope) {
	c.Set("bindingScope", scope)
	c.Set("tenantID", scope.TenantID)
}

func ownTask(id string) *domain.Task {
	return &domain.Task{ID: id, TenantID: bindingTenant, Command: topicA, WorkerID: testSubject, Status: domain.StatusInProgress}
}

func TestCreateTaskBindingRefusalsSkipScheduler(t *testing.T) {
	cases := map[string]struct {
		body map[string]any
		code string
	}{
		"other command": {map[string]any{keyCommand: topicB, keyPayload: 1}, "event_type_not_allowed"},
		"webhook":       {map[string]any{keyCommand: topicA, keyPayload: 1, "webhook": " "}, "webhook_not_allowed"},
	}
	for name, tc := range cases {
		called := false
		svc := &mockSchedulerService{createFunc: func(context.Context, domain.Command, string, int, string, int, string, string, time.Time, int, string) (*domain.Task, error) {
			called = true
			return nil, nil
		}}
		c, rec := newTestContext(t, jsonBody(t, tc.body))
		withScope(c, publishScope())
		NewCreateTaskController(svc).Handle(c)
		if rec.Code != http.StatusForbidden || called || !bodyHasError(t, rec, tc.code) {
			t.Fatalf("%s: status %d called %v body %s", name, rec.Code, called, rec.Body.String())
		}
	}
}

func TestBatchCreateBindingIsAllOrNothing(t *testing.T) {
	var calls int32
	svc := &mockSchedulerService{createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, _ string, _ string, _ time.Time, _ int, tenant string) (*domain.Task, error) {
		atomic.AddInt32(&calls, 1)
		return &domain.Task{ID: "t", Command: cmd, TenantID: tenant}, nil
	}}
	body := map[string]any{keyTasks: []any{
		map[string]any{keyCommand: topicA, keyPayload: 1},
		map[string]any{keyCommand: topicA, keyPayload: 1},
		map[string]any{keyCommand: topicB, keyPayload: 1},
	}}
	c, rec := newTestContext(t, jsonBody(t, body))
	withScope(c, publishScope())
	NewBatchCreateTaskController(svc).Handle(c)
	if rec.Code != http.StatusForbidden || atomic.LoadInt32(&calls) != 0 || !bodyHasError(t, rec, "event_type_not_allowed") {
		t.Fatalf("status %d calls %d body %s", rec.Code, calls, rec.Body.String())
	}
	c, rec = newTestContext(t, jsonBody(t, map[string]any{keyTasks: body[keyTasks].([]any)[:2]}))
	withScope(c, publishScope())
	NewBatchCreateTaskController(svc).Handle(c)
	if rec.Code != http.StatusOK || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("authorized batch: status %d calls %d", rec.Code, calls)
	}
}

type taskScheduler struct {
	mockSchedulerService
	task *domain.Task
	err  error
	hits int32
}

func (s *taskScheduler) GetTask(context.Context, string) (*domain.Task, error) {
	return s.task, s.err
}

func (s *taskScheduler) Heartbeat(context.Context, string, string, int) error {
	atomic.AddInt32(&s.hits, 1)
	return nil
}

func TestGetTaskVisibility(t *testing.T) {
	cases := map[string]struct {
		task   *domain.Task
		err    error
		tenant string
		scope  *authclaims.BindingScope
		status int
	}{
		"own tenant legacy":      {ownTask("t"), nil, bindingTenant, nil, http.StatusOK},
		"other tenant legacy":    {ownTask("t"), nil, "local-tenant", nil, http.StatusNotFound},
		"binding own command":    {ownTask("t"), nil, bindingTenant, publishScope(), http.StatusOK},
		"binding other command":  {&domain.Task{TenantID: bindingTenant, Command: topicB}, nil, bindingTenant, publishScope(), http.StatusNotFound},
		"nil task without error": {nil, nil, bindingTenant, nil, http.StatusNotFound},
		"missing tenant context": {ownTask("t"), nil, "", nil, http.StatusNotFound},
		"lookup error":           {nil, errors.New("boom"), bindingTenant, nil, http.StatusNotFound},
	}
	for name, tc := range cases {
		c, rec := newTestContext(t, jsonBody(t, map[string]any{}))
		c.Set("tenantID", tc.tenant)
		if tc.scope != nil {
			c.Set("bindingScope", tc.scope)
		}
		NewGetTaskController(&taskScheduler{task: tc.task, err: tc.err}).Handle(c)
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d, want %d", name, rec.Code, tc.status)
		}
		if tc.status == http.StatusNotFound && rec.Body.String() != `{"error":"not found"}` {
			t.Fatalf("%s: body %s", name, rec.Body.String())
		}
	}
}

func TestBindingOwnerPrecheck(t *testing.T) {
	cases := map[string]struct {
		task  *domain.Task
		owned bool
	}{
		"owned":         {ownTask("t"), true},
		"other worker":  {&domain.Task{TenantID: bindingTenant, Command: topicA, WorkerID: "codecloud-worker"}, false},
		"empty worker":  {&domain.Task{TenantID: bindingTenant, Command: topicA}, false},
		"other tenant":  {&domain.Task{TenantID: otherTenant, Command: topicA, WorkerID: testSubject}, false},
		"other command": {&domain.Task{TenantID: bindingTenant, Command: topicB, WorkerID: testSubject}, false},
		"missing task":  {nil, false},
	}
	for name, tc := range cases {
		svc := &taskScheduler{task: tc.task}
		c, rec := newTestContext(t, jsonBody(t, map[string]any{}))
		withScope(c, subscribeScope())
		setWorkerClaims(c, testSubject, []string{topicA})
		NewHeartbeatController(svc).Handle(c)
		if tc.owned != (rec.Code == http.StatusOK) || tc.owned != (svc.hits == 1) {
			t.Fatalf("%s: status %d hits %d", name, rec.Code, svc.hits)
		}
		if !tc.owned && !bodyHasError(t, rec, "not-owner") {
			t.Fatalf("%s: body %s", name, rec.Body.String())
		}
	}
}

type resultStub struct {
	mockResultsService
	get     func(n int32) (*domain.ResultRecord, *domain.Task, error)
	gets    int32
	batches int32
	short   bool
}

func (r *resultStub) Get(context.Context, string) (*domain.ResultRecord, *domain.Task, error) {
	return r.get(atomic.AddInt32(&r.gets, 1))
}

func (r *resultStub) BatchSubmit(ctx context.Context, items []domain.BatchSubmitItem) ([]domain.BatchSubmitResponse, error) {
	atomic.AddInt32(&r.batches, 1)
	if r.short {
		return nil, nil
	}
	return r.mockResultsService.BatchSubmit(ctx, items)
}

func TestBatchResultsBindingEdgeCases(t *testing.T) {
	body := map[string]any{keyResults: []any{map[string]any{keyTaskID: "t1", keyStatus: string(domain.StatusCompleted), keyResult: map[string]any{}}}}
	foreign := &resultStub{get: func(int32) (*domain.ResultRecord, *domain.Task, error) {
		return nil, &domain.Task{TenantID: otherTenant}, errors.New("result not found")
	}}
	c, rec := newTestContext(t, jsonBody(t, body))
	withScope(c, subscribeScope())
	setWorkerClaims(c, testSubject, []string{topicA})
	NewBatchSubmitResultController(foreign).Handle(c)
	if rec.Code != http.StatusOK || foreign.batches != 0 || !bodyContains(rec, `"error":"not-owner"`) {
		t.Fatalf("all refused: status %d batches %d body %s", rec.Code, foreign.batches, rec.Body.String())
	}
	short := &resultStub{short: true, get: func(int32) (*domain.ResultRecord, *domain.Task, error) {
		return nil, ownTask("t1"), errors.New("result not found")
	}}
	c, rec = newTestContext(t, jsonBody(t, body))
	withScope(c, subscribeScope())
	setWorkerClaims(c, testSubject, []string{topicA})
	NewBatchSubmitResultController(short).Handle(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("mismatched count: status %d", rec.Code)
	}
}

func TestGetResultLongPollVisibility(t *testing.T) {
	own := ownTask("t")
	foreign := &domain.Task{TenantID: otherTenant, Command: topicA}
	cases := map[string]struct {
		get    func(n int32) (*domain.ResultRecord, *domain.Task, error)
		status int
		body   string
	}{
		"result appears during wait": {func(n int32) (*domain.ResultRecord, *domain.Task, error) {
			if n < 3 {
				return nil, own, errors.New("result not found")
			}
			return &domain.ResultRecord{TaskID: "t"}, own, nil
		}, http.StatusOK, `"result":{"taskId":"t"`},
		"task becomes foreign during wait": {func(n int32) (*domain.ResultRecord, *domain.Task, error) {
			if n < 2 {
				return nil, own, errors.New("result not found")
			}
			return nil, foreign, errors.New("result not found")
		}, http.StatusNotFound, taskNotFoundBody},
		"foreign without result returns immediately": {func(int32) (*domain.ResultRecord, *domain.Task, error) {
			return nil, foreign, errors.New("result not found")
		}, http.StatusNotFound, taskNotFoundBody},
		"result without task is hidden": {func(int32) (*domain.ResultRecord, *domain.Task, error) {
			return &domain.ResultRecord{}, nil, nil
		}, http.StatusNotFound, taskNotFoundBody},
		"deadline with own task": {func(int32) (*domain.ResultRecord, *domain.Task, error) {
			return nil, own, errors.New("result not found")
		}, http.StatusNotFound, `{"error":"result not found"}`},
	}
	for name, tc := range cases {
		gin.SetMode(gin.TestMode)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/?waitSeconds=1", nil)
		c.Set("tenantID", bindingTenant)
		start := time.Now()
		NewGetResultController(&resultStub{get: tc.get}).Handle(c)
		if rec.Code != tc.status || !bodyContains(rec, tc.body) {
			t.Fatalf("%s: status %d body %s", name, rec.Code, rec.Body.String())
		}
		if name == "foreign without result returns immediately" && time.Since(start) > 500*time.Millisecond {
			t.Fatalf("foreign task waited %s", time.Since(start))
		}
	}
}

func bodyHasError(t *testing.T, rec *httptest.ResponseRecorder, code string) bool {
	t.Helper()
	return bodyContains(rec, `"error":"`+code+`"`)
}

func bodyContains(rec *httptest.ResponseRecorder, part string) bool {
	return strings.Contains(rec.Body.String(), part)
}
