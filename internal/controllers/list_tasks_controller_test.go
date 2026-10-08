package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	listTenant  = "tenant-a"
	listCommand = "SYNC"
)

type listCall struct {
	cmd    domain.Command
	tenant string
	state  domain.QueueState
	limit  int
	cursor string
}

func serveList(t *testing.T, query string, page *domain.TaskPage, err error) (*httptest.ResponseRecorder, []listCall) {
	t.Helper()
	var calls []listCall
	svc := &mockSchedulerService{listFunc: func(_ context.Context, cmd domain.Command, tenant string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error) {
		calls = append(calls, listCall{cmd, tenant, state, limit, cursor})
		return page, err
	}}
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/codeq/admin/queues/"+listCommand+"/tasks?"+query, nil)
	c.Params = gin.Params{{Key: keyCommand, Value: listCommand}}
	c.Set("tenantID", listTenant)
	NewListTasksController(svc).Handle(c)
	return rec, calls
}

func TestListTasksControllerPassesQueryAndTenant(t *testing.T) {
	page := &domain.TaskPage{Tasks: []*domain.Task{{ID: "t1", Command: listCommand, TenantID: listTenant}}, NextCursor: "next"}
	rec, calls := serveList(t, "state=inProgress&limit=25&cursor=abc", page, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	want := listCall{listCommand, listTenant, domain.QueueStateInProgress, 25, "abc"}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("service calls %+v, want [%+v]", calls, want)
	}
	var got domain.TaskPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Tasks) != 1 || got.NextCursor != "next" {
		t.Fatalf("body %s (%v)", rec.Body.String(), err)
	}
}

func TestListTasksControllerRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		svcErr      error
		calls       int
	}{
		{"missing state", "", nil, 0},
		{"unknown state", "state=pending", nil, 0},
		{"non-numeric limit", "state=ready&limit=ten", nil, 0},
		{"limit out of range", "state=ready&limit=9999", domain.ErrInvalidListLimit, 1},
		{"foreign cursor", "state=ready&cursor=x", domain.ErrInvalidCursor, 1},
	} {
		rec, calls := serveList(t, tc.query, nil, tc.svcErr)
		if rec.Code != http.StatusBadRequest || len(calls) != tc.calls {
			t.Fatalf("%s: status %d after %d service calls; want 400 after %d", tc.name, rec.Code, len(calls), tc.calls)
		}
	}
	rec, _ := serveList(t, "state=ready", nil, errors.New("disk"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("storage error: status %d, want 500", rec.Code)
	}
}
