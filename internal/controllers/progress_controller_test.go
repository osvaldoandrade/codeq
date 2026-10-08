package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/osvaldoandrade/codeq/internal/middleware"
)

const progressSubject = "worker-1"

// progressScheduler records ReportProgress calls and answers with err.
type progressScheduler struct {
	taskScheduler
	reportErr error
	reports   []json.RawMessage
	workers   []string
}

func (s *progressScheduler) ReportProgress(_ context.Context, _ string, workerID string, progress json.RawMessage) error {
	s.reports = append(s.reports, progress)
	s.workers = append(s.workers, workerID)
	return s.reportErr
}

func postProgress(t *testing.T, svc *progressScheduler, body string, withClaims bool) (int, string) {
	t.Helper()
	c, rec := newTestContext(t, bytes.NewBufferString(body))
	if withClaims {
		setWorkerClaims(c, progressSubject, []string{"*"})
	}
	NewProgressController(svc).Handle(c)
	return rec.Code, rec.Body.String()
}

// stringProgressBody returns a body whose progress is a JSON string of n
// bytes in total (quotes included).
func stringProgressBody(n int) string {
	return `{"progress":"` + strings.Repeat("x", n-2) + `"}`
}

func TestProgressControllerStoresCompactedValue(t *testing.T) {
	svc := &progressScheduler{}
	status, body := postProgress(t, svc, "{\"progress\": { \"done\" : 3,\n \"total\": 10 } }", true)
	if status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("status %d body %s", status, body)
	}
	if len(svc.reports) != 1 || string(svc.reports[0]) != `{"done":3,"total":10}` || svc.workers[0] != progressSubject {
		t.Fatalf("reports %q workers %v", svc.reports, svc.workers)
	}
}

func TestProgressControllerMapsRepositoryErrors(t *testing.T) {
	cases := map[string]int{
		middleware.CodeNotOwner: http.StatusForbidden,
		codeNotFound:            http.StatusNotFound,
		codeNotInProgress:       http.StatusConflict,
		"disk on fire":          http.StatusInternalServerError,
	}
	for msg, want := range cases {
		svc := &progressScheduler{reportErr: errors.New(msg)}
		status, body := postProgress(t, svc, `{"progress":1}`, true)
		if status != want || !strings.Contains(body, `"error":"`+msg+`"`) {
			t.Fatalf("%s: status %d body %s, want %d", msg, status, body, want)
		}
	}
}

func TestProgressControllerRejectsInvalidBodies(t *testing.T) {
	cases := map[string]string{
		"unknown field":    `{"progress":1,"extra":true}`,
		"missing progress": `{}`,
		"null progress":    `{"progress":null}`,
		"two objects":      `{"progress":1}{"progress":2}`,
		"trailing garbage": `{"progress":1} x`,
		"invalid json":     `{"progress":}`,
		"not an object":    `[1]`,
		"empty body":       ``,
	}
	for name, body := range cases {
		svc := &progressScheduler{}
		status, resp := postProgress(t, svc, body, true)
		if status != http.StatusBadRequest || len(svc.reports) != 0 {
			t.Fatalf("%s: status %d body %s reports %d", name, status, resp, len(svc.reports))
		}
	}
}

func TestProgressControllerSizeLimit(t *testing.T) {
	svc := &progressScheduler{}
	if status, body := postProgress(t, svc, stringProgressBody(maxProgressBytes), true); status != http.StatusOK {
		t.Fatalf("value at the limit: status %d body %s", status, body)
	}
	for name, body := range map[string]string{
		"value over the limit":   stringProgressBody(maxProgressBytes + 1),
		"body over the read cap": stringProgressBody(maxProgressRequestBytes + 1),
	} {
		status, resp := postProgress(t, svc, body, true)
		if status != http.StatusRequestEntityTooLarge || resp != `{"error":"progress too large"}` {
			t.Fatalf("%s: status %d body %s", name, status, resp)
		}
	}
	// The limit applies after compaction: whitespace does not count.
	padded := `{"progress":[` + strings.Repeat(" ", maxProgressBytes) + `1]}`
	if status, body := postProgress(t, svc, padded, true); status != http.StatusOK {
		t.Fatalf("padded small value: status %d body %s", status, body)
	}
	if len(svc.reports) != 2 || string(svc.reports[1]) != `[1]` {
		t.Fatalf("reports %d last %q", len(svc.reports), svc.reports[len(svc.reports)-1])
	}
}

func TestProgressControllerRequiresWorkerClaims(t *testing.T) {
	svc := &progressScheduler{}
	if status, _ := postProgress(t, svc, `{"progress":1}`, false); status != http.StatusUnauthorized || len(svc.reports) != 0 {
		t.Fatalf("status %d reports %d", status, len(svc.reports))
	}
}

func TestProgressControllerBindingOwnership(t *testing.T) {
	for name, tc := range map[string]struct {
		owned bool
		want  int
	}{"owner": {true, http.StatusOK}, "other pod": {false, http.StatusForbidden}} {
		task := ownTask("t")
		if !tc.owned {
			task.WorkerID = "other-pod"
		}
		svc := &progressScheduler{taskScheduler: taskScheduler{task: task}}
		c, rec := newTestContext(t, bytes.NewBufferString(`{"progress":{"pct":50}}`))
		withScope(c, subscribeScope())
		setWorkerClaims(c, testSubject, []string{topicA})
		NewProgressController(svc).Handle(c)
		if rec.Code != tc.want || tc.owned != (len(svc.reports) == 1) {
			t.Fatalf("%s: status %d reports %d", name, rec.Code, len(svc.reports))
		}
		if !tc.owned && !bodyHasError(t, rec, middleware.CodeNotOwner) {
			t.Fatalf("%s: body %s", name, rec.Body.String())
		}
	}
}
