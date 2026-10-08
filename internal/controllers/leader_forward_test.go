package controllers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const leaderUnavailableBody = `{"error":"leader_unavailable"}`

func notLeader(url string) error { return &pebble.NotLeaderError{LeaderURL: url} }

// Without a forwarding gate (no Raft), a not-leader error is 503 and
// retryable on every write controller; no controller answers 307.
func TestNotLeaderWithoutForwarderIs503(t *testing.T) {
	sched := &mockSchedulerService{
		createFunc: func(context.Context, domain.Command, string, int, string, int, string, string, string, time.Time, int, string) (*domain.Task, error) {
			return nil, notLeader("http://codeq-1:8080")
		},
		claimFunc: func(context.Context, string, []domain.Command, int, int, string) (*domain.Task, bool, error) {
			return nil, false, notLeader("http://codeq-1:8080")
		},
	}
	cases := map[string]struct {
		handler gin.HandlerFunc
		body    string
	}{
		"create":      {NewCreateTaskController(sched).Handle, `{"command":"c","payload":1}`},
		"claim":       {NewClaimTaskController(sched).Handle, `{}`},
		"batch claim": {NewBatchClaimTaskController(sched).Handle, `{"count":2}`},
	}
	for name, tc := range cases {
		ctx, rec := newTestContext(t, jsonBody(t, nil))
		ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		setWorkerClaims(ctx, "worker-1", []string{"*"})
		tc.handler(ctx)
		if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != leaderUnavailableBody {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: headers %v", name, rec.Header())
		}
	}
}

// A batch claim that claimed nothing locally forwards the whole request
// when leadership moved after the route gate; one that already claimed
// tasks keeps its partial answer and never re-runs on the leader.
func TestBatchClaimForwardsOnlyWhenNothingClaimed(t *testing.T) {
	var hits int
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"count":2}` || r.Header.Get(leaderforward.HeaderForwarded) != "1" {
			t.Errorf("leader saw body %q headers %v", b, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[{"id":"from-leader"}]}`))
	}))
	defer leader.Close()
	fwd := leaderforward.New(leaderforward.Config{
		PeerHTTPAddrs: map[string]string{"codeq-0": "http://self:8080", "codeq-1": leader.URL},
		SelfID:        "codeq-0",
		Groups:        []leaderforward.Leadership{standalone{}},
	})
	run := func(claimed int) *httptest.ResponseRecorder {
		calls := 0
		sched := &mockSchedulerService{claimFunc: func(context.Context, string, []domain.Command, int, int, string) (*domain.Task, bool, error) {
			calls++
			if calls <= claimed {
				return &domain.Task{ID: "local"}, true, nil
			}
			return nil, false, notLeader(leader.URL)
		}}
		gin.SetMode(gin.TestMode)
		e := gin.New()
		e.POST("/v1/codeq/tasks/claim/batch", func(c *gin.Context) { setWorkerClaims(c, "worker-1", []string{"*"}) }, fwd.Batch(), NewBatchClaimTaskController(sched).Handle)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks/claim/batch", strings.NewReader(`{"count":2}`)))
		return rec
	}
	if rec := run(0); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from-leader") || hits != 1 {
		t.Fatalf("nothing claimed: %d %s hits=%d", rec.Code, rec.Body.String(), hits)
	}
	if rec := run(1); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"local"`) || hits != 1 {
		t.Fatalf("partial claim: %d %s hits=%d", rec.Code, rec.Body.String(), hits)
	}
}

// standalone reports local write leadership, so the route gate lets the
// controller run and only the controller's not-leader error forwards.
type standalone struct{}

func (standalone) RequireWriteLeader() error { return nil }
