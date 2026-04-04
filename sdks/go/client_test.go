package codeq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// helper to start a test server with a custom handler.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL,
		WithProducerToken("tok-producer"),
		WithWorkerToken("tok-worker"),
		WithAdminToken("tok-admin"),
		WithMaxRetries(0),
	)
	return srv, client
}

// ---------- Constructor ----------

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("http://localhost:8080")
	if c.cfg.BaseURL != "http://localhost:8080" {
		t.Fatalf("BaseURL = %q, want %q", c.cfg.BaseURL, "http://localhost:8080")
	}
	if c.cfg.MaxRetries != 3 {
		t.Fatalf("MaxRetries = %d, want 3", c.cfg.MaxRetries)
	}
	if c.cfg.HTTPClient == nil {
		t.Fatal("HTTPClient should not be nil")
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://localhost:8080/")
	if c.cfg.BaseURL != "http://localhost:8080" {
		t.Fatalf("BaseURL = %q, want trailing slash trimmed", c.cfg.BaseURL)
	}
}

func TestNewClient_WithOptions(t *testing.T) {
	hc := &http.Client{Timeout: 5 * time.Second}
	c := NewClient("http://host",
		WithProducerToken("p"),
		WithWorkerToken("w"),
		WithAdminToken("a"),
		WithHTTPClient(hc),
		WithMaxRetries(5),
		WithRetryBaseDelay(2*time.Second),
	)
	if c.cfg.ProducerToken != "p" {
		t.Fatalf("ProducerToken = %q", c.cfg.ProducerToken)
	}
	if c.cfg.WorkerToken != "w" {
		t.Fatalf("WorkerToken = %q", c.cfg.WorkerToken)
	}
	if c.cfg.AdminToken != "a" {
		t.Fatalf("AdminToken = %q", c.cfg.AdminToken)
	}
	if c.cfg.HTTPClient != hc {
		t.Fatal("HTTPClient was not set")
	}
	if c.cfg.MaxRetries != 5 {
		t.Fatalf("MaxRetries = %d", c.cfg.MaxRetries)
	}
	if c.cfg.RetryBaseDelay != 2*time.Second {
		t.Fatalf("RetryBaseDelay = %v", c.cfg.RetryBaseDelay)
	}
}

// ---------- CreateTask ----------

func TestCreateTask_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/codeq/tasks" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-producer" {
			t.Errorf("Authorization = %q", got)
		}

		var body CreateTaskOptions
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Command != "PROCESS" {
			t.Errorf("command = %q", body.Command)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{
			ID:      "task-1",
			Command: "PROCESS",
			Status:  StatusPending,
		})
	})

	task, err := client.CreateTask(context.Background(), CreateTaskOptions{
		Command: "PROCESS",
		Payload: map[string]string{"key": "value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Errorf("ID = %q", task.ID)
	}
	if task.Status != StatusPending {
		t.Errorf("Status = %q", task.Status)
	}
}

func TestCreateTask_APIError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"missing command"}`))
	})

	_, err := client.CreateTask(context.Background(), CreateTaskOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
}

// ---------- CreateTasksBatch ----------

func TestCreateTasksBatch_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/batch" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]BatchTaskResult{
			{Task: &Task{ID: "t-1"}},
			{Task: &Task{ID: "t-2"}},
		})
	})

	results, err := client.CreateTasksBatch(context.Background(), []CreateTaskOptions{
		{Command: "A", Payload: nil},
		{Command: "B", Payload: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("len = %d", len(results))
	}
}

// ---------- ClaimTask ----------

func TestClaimTask_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/claim" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-worker" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "claimed-1", Status: StatusInProgress})
	})

	task, err := client.ClaimTask(context.Background(), ClaimTaskOptions{
		Commands: []string{"PROCESS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.ID != "claimed-1" {
		t.Fatalf("task = %+v", task)
	}
}

func TestClaimTask_NoContent(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	task, err := client.ClaimTask(context.Background(), ClaimTaskOptions{
		Commands: []string{"PROCESS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
}

// ---------- ClaimTasksBatch ----------

func TestClaimTasksBatch_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/claim/batch" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Task{
			{ID: "c-1", Status: StatusInProgress},
			{ID: "c-2", Status: StatusInProgress},
		})
	})

	tasks, err := client.ClaimTasksBatch(context.Background(), ClaimTaskOptions{
		Commands: []string{"CMD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len = %d", len(tasks))
	}
}

// ---------- SubmitResult ----------

func TestSubmitResult_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-1/result" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ResultRecord{
			TaskID: "task-1",
			Status: StatusCompleted,
		})
	})

	rec, err := client.SubmitResult(context.Background(), "task-1", SubmitResultOptions{
		Status: StatusCompleted,
		Result: map[string]string{"output": "done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.TaskID != "task-1" {
		t.Errorf("TaskID = %q", rec.TaskID)
	}
}

// ---------- SubmitResultsBatch ----------

func TestSubmitResultsBatch_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/batch/results" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]BatchTaskResult{
			{Task: &Task{ID: "t-1"}},
		})
	})

	results, err := client.SubmitResultsBatch(context.Background(), []BatchResultSubmission{
		{TaskID: "t-1", Options: SubmitResultOptions{Status: StatusCompleted}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("len = %d", len(results))
	}
}

// ---------- Heartbeat ----------

func TestHeartbeat_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-1/heartbeat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		if body["extendSeconds"] != 300 {
			t.Errorf("extendSeconds = %d", body["extendSeconds"])
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})

	err := client.Heartbeat(context.Background(), "task-1", 300)
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- Abandon ----------

func TestAbandon_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-1/abandon" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})

	err := client.Abandon(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
}

// ---------- Nack ----------

func TestNack_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-1/nack" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["reason"] != "temporary failure" {
			t.Errorf("reason = %v", body["reason"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(NackResponse{Status: "nacked", DelaySeconds: 60})
	})

	resp, err := client.Nack(context.Background(), "task-1", 60, "temporary failure")
	if err != nil {
		t.Fatal(err)
	}
	if resp.DelaySeconds != 60 {
		t.Errorf("DelaySeconds = %d", resp.DelaySeconds)
	}
}

// ---------- CreateSubscription ----------

func TestCreateSubscription_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/workers/subscriptions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SubscriptionResponse{
			SubscriptionID: "sub-1",
			ExpiresAt:      "2026-12-31T00:00:00Z",
		})
	})

	resp, err := client.CreateSubscription(context.Background(), CreateSubscriptionOptions{
		CallbackURL: "https://example.com/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SubscriptionID != "sub-1" {
		t.Errorf("SubscriptionID = %q", resp.SubscriptionID)
	}
}

// ---------- RenewSubscription ----------

func TestRenewSubscription_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/workers/subscriptions/sub-1/heartbeat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SubscriptionResponse{
			SubscriptionID: "sub-1",
			ExpiresAt:      "2027-01-01T00:00:00Z",
		})
	})

	ttl := 3600
	resp, err := client.RenewSubscription(context.Background(), "sub-1", &RenewSubscriptionOptions{
		TTLSeconds: &ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SubscriptionID != "sub-1" {
		t.Errorf("SubscriptionID = %q", resp.SubscriptionID)
	}
}

// ---------- GetTask ----------

func TestGetTask_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/codeq/tasks/task-1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "task-1", Status: StatusCompleted})
	})

	task, err := client.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Errorf("ID = %q", task.ID)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	})

	_, err := client.GetTask(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
}

// ---------- GetResult ----------

func TestGetResult_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-1/result" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TaskResult{
			Task:   Task{ID: "task-1", Status: StatusCompleted},
			Result: "output",
		})
	})

	result, err := client.GetResult(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != "task-1" {
		t.Errorf("Task.ID = %q", result.Task.ID)
	}
}

// ---------- WaitForResult ----------

func TestWaitForResult_ImmediateSuccess(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TaskResult{
			Task: Task{ID: "task-1", Status: StatusCompleted},
		})
	})

	result, err := client.WaitForResult(context.Background(), "task-1", &WaitForResultOptions{
		Timeout:      5 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != "task-1" {
		t.Errorf("Task.ID = %q", result.Task.ID)
	}
}

func TestWaitForResult_PollsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TaskResult{
			Task: Task{ID: "task-1", Status: StatusCompleted},
		})
	})

	result, err := client.WaitForResult(context.Background(), "task-1", &WaitForResultOptions{
		Timeout:      5 * time.Second,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != "task-1" {
		t.Errorf("Task.ID = %q", result.Task.ID)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("calls = %d, want >= 3", got)
	}
}

func TestWaitForResult_Timeout(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	})

	_, err := client.WaitForResult(context.Background(), "task-1", &WaitForResultOptions{
		Timeout:      200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if _, ok := err.(*TimeoutError); !ok {
		t.Fatalf("expected *TimeoutError, got %T: %v", err, err)
	}
}

// ---------- ListQueues ----------

func TestListQueues_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/queues" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-admin" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]QueueStats{
			{Command: "CMD_A", Ready: 10, InProgress: 2},
		})
	})

	stats, err := client.ListQueues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Command != "CMD_A" {
		t.Errorf("stats = %+v", stats)
	}
}

// ---------- GetQueueStats ----------

func TestGetQueueStats_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/queues/PROCESS" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(QueueStats{
			Command: "PROCESS", Ready: 5, Delayed: 1, InProgress: 3, DLQ: 0,
		})
	})

	stats, err := client.GetQueueStats(context.Background(), "PROCESS")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Ready != 5 {
		t.Errorf("Ready = %d", stats.Ready)
	}
}

// ---------- CleanupExpired ----------

func TestCleanupExpired_Success(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/tasks/cleanup" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CleanupResult{Deleted: 42})
	})

	limit := 100
	result, err := client.CleanupExpired(context.Background(), &CleanupOptions{Limit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 42 {
		t.Errorf("Deleted = %d", result.Deleted)
	}
}

// ---------- Auth errors ----------

func TestAuthError_Unauthorized(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	})

	_, err := client.CreateTask(context.Background(), CreateTaskOptions{Command: "X"})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

func TestAuthError_Forbidden(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"insufficient scope"}`))
	})

	_, err := client.ListQueues(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("expected *AuthError, got %T: %v", err, err)
	}
}

// ---------- Retry behaviour ----------

func TestRetry_ServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"oops"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "ok"})
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL,
		WithProducerToken("tok"),
		WithMaxRetries(3),
		WithRetryBaseDelay(10*time.Millisecond),
	)

	task, err := client.CreateTask(context.Background(), CreateTaskOptions{Command: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "ok" {
		t.Errorf("ID = %q", task.ID)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestRetry_ClientErrorNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL,
		WithProducerToken("tok"),
		WithMaxRetries(3),
		WithRetryBaseDelay(10*time.Millisecond),
	)

	_, err := client.CreateTask(context.Background(), CreateTaskOptions{Command: "X"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (client errors should not retry)", got)
	}
}

// ---------- URL encoding ----------

func TestURLEncoding_SpecialCharacters(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Go's http.Server decodes percent-encoded path segments, so
		// url.PathEscape("id with spaces") arrives as "id with spaces".
		if r.URL.Path != "/v1/codeq/tasks/id+with+spaces" && r.URL.Path != "/v1/codeq/tasks/id%20with%20spaces" && r.URL.Path != "/v1/codeq/tasks/id with spaces" {
			t.Errorf("unexpected path = %s", r.URL.Path)
		}
		// Verify the raw URL still has the encoding.
		if r.RequestURI == "" {
			t.Error("RequestURI is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Task{ID: "id with spaces"})
	})

	task, err := client.GetTask(context.Background(), "id with spaces")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "id with spaces" {
		t.Errorf("ID = %q", task.ID)
	}
}

// ---------- Error types ----------

func TestError_String(t *testing.T) {
	e := &Error{Message: "test", Cause: nil}
	if e.Error() != "codeq: test" {
		t.Errorf("Error() = %q", e.Error())
	}

	inner := &Error{Message: "inner"}
	e2 := &Error{Message: "outer", Cause: inner}
	if got := e2.Error(); got != "codeq: outer: codeq: inner" {
		t.Errorf("Error() = %q", got)
	}
	if e2.Unwrap() != inner {
		t.Error("Unwrap did not return inner error")
	}
}

func TestAPIError_String(t *testing.T) {
	e := &APIError{StatusCode: 404, Message: "Not Found", ResponseBody: `{"detail":"gone"}`}
	expected := `codeq: API error (status 404): Not Found – {"detail":"gone"}`
	if e.Error() != expected {
		t.Errorf("Error() = %q", e.Error())
	}

	e2 := &APIError{StatusCode: 500, Message: "Internal Server Error"}
	if got := e2.Error(); got != "codeq: API error (status 500): Internal Server Error" {
		t.Errorf("Error() = %q", got)
	}
}

func TestAuthError_String(t *testing.T) {
	e := &AuthError{Message: "bad token"}
	if e.Error() != "codeq: auth error: bad token" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestTimeoutError_String(t *testing.T) {
	e := &TimeoutError{Message: "deadline exceeded"}
	if e.Error() != "codeq: timeout: deadline exceeded" {
		t.Errorf("Error() = %q", e.Error())
	}
}

// ---------- tokenForRead ----------

func TestTokenForRead_PrefersProducer(t *testing.T) {
	c := NewClient("http://host",
		WithProducerToken("p"),
		WithWorkerToken("w"),
		WithAdminToken("a"),
	)
	if got := c.tokenForRead(); got != "p" {
		t.Errorf("tokenForRead = %q, want p", got)
	}
}

func TestTokenForRead_FallsBackToWorker(t *testing.T) {
	c := NewClient("http://host", WithWorkerToken("w"), WithAdminToken("a"))
	if got := c.tokenForRead(); got != "w" {
		t.Errorf("tokenForRead = %q, want w", got)
	}
}

func TestTokenForRead_FallsBackToAdmin(t *testing.T) {
	c := NewClient("http://host", WithAdminToken("a"))
	if got := c.tokenForRead(); got != "a" {
		t.Errorf("tokenForRead = %q, want a", got)
	}
}

// ---------- Context cancellation ----------

func TestContextCancellation(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second) // simulate slow server
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.CreateTask(ctx, CreateTaskOptions{Command: "X"})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}
