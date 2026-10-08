package schedules

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
)

const testTenant = "acme"

type fakeService struct {
	spec    schedule.Spec
	created bool
	err     error
}

func (f *fakeService) Upsert(_ context.Context, tenant, name string, spec schedule.Spec) (schedule.Schedule, bool, error) {
	f.spec = spec
	return schedule.Schedule{ScheduleID: tenant + "." + name, TenantID: tenant, Name: name, Spec: spec}, f.created, f.err
}

func (f *fakeService) Get(_ context.Context, tenant, name string) (schedule.Schedule, error) {
	return schedule.Schedule{ScheduleID: tenant + "." + name}, f.err
}

func (f *fakeService) List(context.Context, string) ([]schedule.Schedule, error) {
	return []schedule.Schedule{}, f.err
}

func (f *fakeService) Delete(context.Context, string, string) error { return f.err }

func serve(t *testing.T, svc Service, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(svc)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("tenantID", testTenant) })
	r.PUT("/schedules/:name", h.Upsert)
	r.GET("/schedules/:name", h.Get)
	r.GET("/schedules", h.List)
	r.DELETE("/schedules/:name", h.Delete)
	path := "/schedules/nightly"
	if method == "LIST" {
		method, path = http.MethodGet, "/schedules"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestUpsertStatusAndStrictDecoding(t *testing.T) {
	svc := &fakeService{created: true}
	rec := serve(t, svc, http.MethodPut, `{"cron":"@hourly","command":"SYNC","payload":{"a":1}}`)
	if rec.Code != http.StatusCreated || svc.spec.Command != "SYNC" || string(svc.spec.Payload) != `{"a":1}` {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	svc.created = false
	if rec := serve(t, svc, http.MethodPut, `{"cron":"@hourly","command":"SYNC"}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %d", rec.Code)
	}
	for _, body := range []string{`{`, `{"cron":"@hourly","bogus":1}`, `{"cron":"@hourly"}{"cron":"@daily"}`} {
		if rec := serve(t, svc, http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{&schedule.ValidationError{Field: "cron", Message: "bad"}, http.StatusUnprocessableEntity},
		{&schedule.NotFoundError{ScheduleID: "acme.nightly"}, http.StatusNotFound},
		{&schedule.UnavailableError{Reason: "cluster"}, http.StatusServiceUnavailable},
		{errors.New("disk"), http.StatusInternalServerError},
	} {
		svc := &fakeService{err: tc.err}
		for _, method := range []string{http.MethodGet, http.MethodDelete, "LIST"} {
			if rec := serve(t, svc, method, ""); rec.Code != tc.status {
				t.Fatalf("%s with %T: status %d, want %d", method, tc.err, rec.Code, tc.status)
			}
		}
	}
	rec := serve(t, &fakeService{err: &schedule.ValidationError{Field: "timezone", Message: "bad"}}, http.MethodPut, `{"cron":"@hourly","command":"S"}`)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["field"] != "timezone" {
		t.Fatalf("422 body %s must name the field", rec.Body.String())
	}
}

func TestListAndDeleteShapes(t *testing.T) {
	rec := serve(t, &fakeService{}, "LIST", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"schedules":[]}` {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(t, &fakeService{}, http.MethodDelete, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
}
