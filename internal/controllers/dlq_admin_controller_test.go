package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	dlqAdminTenant = "tenant-a"
	dlqAdminTaskID = "t-1"
	dlqAdminCmd    = "SYNC"
)

// adminTaskService returns a mock whose GetTask finds dlqAdminTaskID in
// owner's tenant (or fails with getErr) and whose admin operations return
// opErr.
func adminTaskService(owner string, getErr, opErr error, calls *int) *mockSchedulerService {
	return &mockSchedulerService{
		getTaskFunc: func(_ context.Context, id string) (*domain.Task, error) {
			if getErr != nil {
				return nil, getErr
			}
			if id != dlqAdminTaskID {
				return nil, errors.New("not-found")
			}
			return &domain.Task{ID: id, Command: dlqAdminCmd, TenantID: owner}, nil
		},
		requeueTaskFunc: func(_ context.Context, id string) (*domain.Task, error) {
			*calls++
			if opErr != nil {
				return nil, opErr
			}
			return &domain.Task{ID: id, Status: domain.StatusPending, TenantID: owner}, nil
		},
		deleteTaskFunc: func(context.Context, string) error {
			*calls++
			return opErr
		},
	}
}

func serveAdmin(t *testing.T, method, path string, params gin.Params, handle gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = params
	c.Set("tenantID", dlqAdminTenant)
	handle(c)
	c.Writer.WriteHeaderNow() // the engine does this after the last handler
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body[errorField]
}

func TestAdminTaskControllersMapEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name          string
		owner, id     string
		getErr, opErr error
		requeueStatus int
		deleteStatus  int
		errCode       string
		calls         int
	}{
		{"success", dlqAdminTenant, dlqAdminTaskID, nil, nil, http.StatusOK, http.StatusNoContent, "", 1},
		{"missing task", dlqAdminTenant, "other", nil, nil, http.StatusNotFound, http.StatusNotFound, adminNotFoundBody, 0},
		{"other tenant", "tenant-b", dlqAdminTaskID, nil, nil, http.StatusNotFound, http.StatusNotFound, adminNotFoundBody, 0},
		{"read failure", dlqAdminTenant, dlqAdminTaskID, errors.New("disk"), nil, http.StatusInternalServerError, http.StatusInternalServerError, "disk", 0},
		{"gone before the write", dlqAdminTenant, dlqAdminTaskID, nil, errors.New("not-found"), http.StatusNotFound, http.StatusNotFound, adminNotFoundBody, 1},
		{"write failure", dlqAdminTenant, dlqAdminTaskID, nil, errors.New("disk"), http.StatusInternalServerError, http.StatusInternalServerError, "disk", 1},
	} {
		params := gin.Params{{Key: "id", Value: tc.id}}
		calls := 0
		svc := adminTaskService(tc.owner, tc.getErr, tc.opErr, &calls)
		requeue := serveAdmin(t, http.MethodPost, "/v1/codeq/admin/tasks/"+tc.id+"/requeue", params, NewRequeueTaskController(svc).Handle)
		del := serveAdmin(t, http.MethodDelete, "/v1/codeq/admin/tasks/"+tc.id, params, NewDeleteTaskController(svc).Handle)
		if requeue.Code != tc.requeueStatus || del.Code != tc.deleteStatus {
			t.Fatalf("%s: requeue %d delete %d; want %d and %d", tc.name, requeue.Code, del.Code, tc.requeueStatus, tc.deleteStatus)
		}
		if calls != 2*tc.calls {
			t.Fatalf("%s: %d service writes, want %d", tc.name, calls, 2*tc.calls)
		}
		if tc.errCode != "" && (errorBody(t, requeue) != tc.errCode || errorBody(t, del) != tc.errCode) {
			t.Fatalf("%s: bodies %s / %s, want error %q", tc.name, requeue.Body.String(), del.Body.String(), tc.errCode)
		}
		if tc.name == "success" {
			var task domain.Task
			if err := json.Unmarshal(requeue.Body.Bytes(), &task); err != nil || task.Status != domain.StatusPending {
				t.Fatalf("requeue body %s", requeue.Body.String())
			}
			if del.Body.Len() != 0 {
				t.Fatalf("delete body %q, want none", del.Body.String())
			}
		}
	}
}

func TestAdminTaskControllersConflicts(t *testing.T) {
	params := gin.Params{{Key: "id", Value: dlqAdminTaskID}}
	calls := 0
	rec := serveAdmin(t, http.MethodPost, "/", params, NewRequeueTaskController(adminTaskService(dlqAdminTenant, nil, domain.ErrTaskNotInDLQ, &calls)).Handle)
	if rec.Code != http.StatusConflict || errorBody(t, rec) != "task_not_in_dlq" {
		t.Fatalf("requeue outside the DLQ: %d %s", rec.Code, rec.Body.String())
	}
	rec = serveAdmin(t, http.MethodDelete, "/", params, NewDeleteTaskController(adminTaskService(dlqAdminTenant, nil, domain.ErrTaskInProgress, &calls)).Handle)
	if rec.Code != http.StatusConflict || errorBody(t, rec) != "task_in_progress" {
		t.Fatalf("delete in progress: %d %s", rec.Code, rec.Body.String())
	}
}

// Without a forwarding context a not-leader error is 503 leader_unavailable
// on every admin write, never a 500.
func TestAdminDLQControllersAnswerNotLeader(t *testing.T) {
	notLeader := &fakeLeaderHint{url: "http://leader"}
	calls := 0
	params := gin.Params{{Key: "id", Value: dlqAdminTaskID}, {Key: keyCommand, Value: dlqAdminCmd}}
	svc := adminTaskService(dlqAdminTenant, nil, notLeader, &calls)
	svc.requeueDLQFunc = func(context.Context, domain.Command, string, int) (*domain.DLQRequeue, error) {
		return nil, notLeader
	}
	for name, handle := range map[string]gin.HandlerFunc{
		"requeue":      NewRequeueTaskController(svc).Handle,
		"delete":       NewDeleteTaskController(svc).Handle,
		"bulk requeue": NewRequeueDLQController(svc).Handle,
	} {
		rec := serveAdmin(t, http.MethodPost, "/", params, handle)
		if rec.Code != http.StatusServiceUnavailable || errorBody(t, rec) != leaderforward.CodeLeaderUnavailable {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

type fakeLeaderHint struct{ url string }

func (f *fakeLeaderHint) Error() string          { return "not leader" }
func (f *fakeLeaderHint) LeaderHTTPAddr() string { return f.url }

type bulkCall struct {
	cmd    domain.Command
	tenant string
	limit  int
}

func serveBulk(t *testing.T, command, query string, out *domain.DLQRequeue, err error) (*httptest.ResponseRecorder, []bulkCall) {
	t.Helper()
	var calls []bulkCall
	svc := &mockSchedulerService{requeueDLQFunc: func(_ context.Context, cmd domain.Command, tenant string, limit int) (*domain.DLQRequeue, error) {
		calls = append(calls, bulkCall{cmd, tenant, limit})
		return out, err
	}}
	rec := serveAdmin(t, http.MethodPost, "/v1/codeq/admin/queues/"+url.PathEscape(command)+"/dlq/requeue?"+query,
		gin.Params{{Key: keyCommand, Value: command}}, NewRequeueDLQController(svc).Handle)
	return rec, calls
}

func TestRequeueDLQControllerPassesLimitAndTenant(t *testing.T) {
	rec, calls := serveBulk(t, dlqAdminCmd, "limit=250", &domain.DLQRequeue{Requeued: 250, Remaining: true}, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"requeued":250,"remaining":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if want := (bulkCall{dlqAdminCmd, dlqAdminTenant, 250}); len(calls) != 1 || calls[0] != want {
		t.Fatalf("service calls %+v, want [%+v]", calls, want)
	}
	if _, calls = serveBulk(t, dlqAdminCmd, "", &domain.DLQRequeue{}, nil); len(calls) != 1 || calls[0].limit != 0 {
		t.Fatalf("no limit: service calls %+v, want limit 0 (service default)", calls)
	}
}

func TestRequeueDLQControllerRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name, command, query string
		svcErr               error
		status, calls        int
	}{
		{"blank command", " ", "", nil, http.StatusBadRequest, 0},
		{"non-numeric limit", dlqAdminCmd, "limit=ten", nil, http.StatusBadRequest, 0},
		{"limit out of range", dlqAdminCmd, "limit=5000", domain.ErrInvalidRequeueLimit, http.StatusBadRequest, 1},
		{"storage failure", dlqAdminCmd, "", errors.New("disk"), http.StatusInternalServerError, 1},
	} {
		rec, calls := serveBulk(t, tc.command, tc.query, nil, tc.svcErr)
		if rec.Code != tc.status || len(calls) != tc.calls {
			t.Fatalf("%s: status %d after %d service calls; want %d after %d", tc.name, rec.Code, len(calls), tc.status, tc.calls)
		}
	}
}
