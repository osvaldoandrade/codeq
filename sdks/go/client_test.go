package codeq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ──────────────────────────────────────────────
// Helper
// ──────────────────────────────────────────────

func setupServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL,
		WithProducerToken("prod-token"),
		WithWorkerToken("work-token"),
		WithAdminToken("admin-token"),
	)
	return srv, client
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ──────────────────────────────────────────────
// NewClient
// ──────────────────────────────────────────────

func TestNewClient(t *testing.T) {
	c := NewClient("http://localhost:8080/",
		WithProducerToken("p"),
		WithWorkerToken("w"),
		WithAdminToken("a"),
	)
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash stripped, got %s", c.baseURL)
	}
	if c.producerToken != "p" {
		t.Errorf("expected producerToken 'p', got %s", c.producerToken)
	}
	if c.workerToken != "w" {
		t.Errorf("expected workerToken 'w', got %s", c.workerToken)
	}
	if c.adminToken != "a" {
		t.Errorf("expected adminToken 'a', got %s", c.adminToken)
	}
}

func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 5 * time.Second}
	c := NewClient("http://localhost:8080", WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Error("expected custom HTTP client to be set")
	}
}

// ──────────────────────────────────────────────
// CreateTask
// ──────────────────────────────────────────────

func TestCreateTask(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/codeq/tasks" {
			t.Errorf("expected /v1/codeq/tasks, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer prod-token" {
			t.Errorf("expected producer token, got %s", r.Header.Get("Authorization"))
		}

		var body CreateTaskOptions
		json.NewDecoder(r.Body).Decode(&body)
		if body.Command != "GENERATE_MASTER" {
			t.Errorf("expected command GENERATE_MASTER, got %s", body.Command)
		}

		jsonResponse(w, http.StatusCreated, Task{
			ID:      "task-123",
			Command: "GENERATE_MASTER",
			Status:  StatusPending,
		})
	})

	task, err := client.CreateTask(context.Background(), &CreateTaskOptions{
		Command:  "GENERATE_MASTER",
		Payload:  map[string]any{"jobId": "123"},
		Priority: Int(5),
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "task-123" {
		t.Errorf("expected task ID 'task-123', got %s", task.ID)
	}
	if task.Status != StatusPending {
		t.Errorf("expected status PENDING, got %s", task.Status)
	}
}

func TestCreateTask_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.CreateTask(context.Background(), &CreateTaskOptions{Command: "CMD"})
	if err == nil {
		t.Fatal("expected error for missing producer token")
	}
}

func TestCreateTask_ServerError(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	})

	_, err := client.CreateTask(context.Background(), &CreateTaskOptions{Command: "CMD"})
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

// ──────────────────────────────────────────────
// CreateTasksBatch
// ──────────────────────────────────────────────

func TestCreateTasksBatch(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/batch" {
			t.Errorf("expected /v1/codeq/tasks/batch, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, []BatchCreateResult{
			{Task: &Task{ID: "t-1"}},
			{Task: &Task{ID: "t-2"}},
		})
	})

	results, err := client.CreateTasksBatch(context.Background(), []CreateTaskOptions{
		{Command: "CMD1", Payload: "a"},
		{Command: "CMD2", Payload: "b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Task.ID != "t-1" {
		t.Errorf("expected first task ID 't-1', got %s", results[0].Task.ID)
	}
}

// ──────────────────────────────────────────────
// ClaimTask
// ──────────────────────────────────────────────

func TestClaimTask(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/claim" {
			t.Errorf("expected /v1/codeq/tasks/claim, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer work-token" {
			t.Errorf("expected worker token")
		}

		jsonResponse(w, http.StatusOK, Task{
			ID:      "task-456",
			Command: "GENERATE_MASTER",
			Status:  StatusInProgress,
		})
	})

	task, err := client.ClaimTask(context.Background(), &ClaimTaskOptions{
		Commands:     []string{"GENERATE_MASTER"},
		LeaseSeconds: Int(120),
		WaitSeconds:  Int(10),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "task-456" {
		t.Errorf("expected task ID 'task-456', got %s", task.ID)
	}
}

func TestClaimTask_NoContent(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	task, err := client.ClaimTask(context.Background(), &ClaimTaskOptions{
		Commands: []string{"CMD"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != nil {
		t.Error("expected nil task for 204 response")
	}
}

func TestClaimTask_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.ClaimTask(context.Background(), &ClaimTaskOptions{Commands: []string{"CMD"}})
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// ClaimTasksBatch
// ──────────────────────────────────────────────

func TestClaimTasksBatch(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/claim/batch" {
			t.Errorf("expected /v1/codeq/tasks/claim/batch, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, []Task{
			{ID: "t-1", Status: StatusInProgress},
			{ID: "t-2", Status: StatusInProgress},
		})
	})

	tasks, err := client.ClaimTasksBatch(context.Background(), &BatchClaimOptions{
		Commands: []string{"CMD"},
		Count:    2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestClaimTasksBatch_NoContent(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	tasks, err := client.ClaimTasksBatch(context.Background(), &BatchClaimOptions{Count: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tasks != nil {
		t.Error("expected nil for 204 response")
	}
}

// ──────────────────────────────────────────────
// SubmitResult
// ──────────────────────────────────────────────

func TestSubmitResult(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-789/result" {
			t.Errorf("expected /v1/codeq/tasks/task-789/result, got %s", r.URL.Path)
		}

		var body SubmitResultOptions
		json.NewDecoder(r.Body).Decode(&body)
		if body.Status != "COMPLETED" {
			t.Errorf("expected status COMPLETED, got %s", body.Status)
		}

		jsonResponse(w, http.StatusOK, ResultRecord{
			TaskID: "task-789",
			Status: StatusCompleted,
		})
	})

	rec, err := client.SubmitResult(context.Background(), "task-789", &SubmitResultOptions{
		Status: "COMPLETED",
		Result: map[string]any{"output": "done"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.TaskID != "task-789" {
		t.Errorf("expected task ID 'task-789', got %s", rec.TaskID)
	}
}

func TestSubmitResult_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.SubmitResult(context.Background(), "id", &SubmitResultOptions{Status: "COMPLETED"})
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// SubmitResultsBatch
// ──────────────────────────────────────────────

func TestSubmitResultsBatch(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/batch/results" {
			t.Errorf("expected /v1/codeq/tasks/batch/results, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, []BatchSubmitResult{
			{TaskID: "t-1", Result: &ResultRecord{TaskID: "t-1", Status: StatusCompleted}},
		})
	})

	results, err := client.SubmitResultsBatch(context.Background(), []BatchSubmitItem{
		{TaskID: "t-1", SubmitResultOptions: SubmitResultOptions{Status: "COMPLETED"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

// ──────────────────────────────────────────────
// Heartbeat
// ──────────────────────────────────────────────

func TestHeartbeat(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-abc/heartbeat" {
			t.Errorf("expected /v1/codeq/tasks/task-abc/heartbeat, got %s", r.URL.Path)
		}

		var body struct{ ExtendSeconds int }
		json.NewDecoder(r.Body).Decode(&body)
		if body.ExtendSeconds != 120 {
			t.Errorf("expected extendSeconds 120, got %d", body.ExtendSeconds)
		}

		w.WriteHeader(http.StatusOK)
	})

	err := client.Heartbeat(context.Background(), "task-abc", 120)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHeartbeat_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	err := c.Heartbeat(context.Background(), "id", 120)
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// Abandon
// ──────────────────────────────────────────────

func TestAbandon(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-abc/abandon" {
			t.Errorf("expected /v1/codeq/tasks/task-abc/abandon, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.Abandon(context.Background(), "task-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAbandon_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	err := c.Abandon(context.Background(), "id")
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// Nack
// ──────────────────────────────────────────────

func TestNack(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-abc/nack" {
			t.Errorf("expected /v1/codeq/tasks/task-abc/nack, got %s", r.URL.Path)
		}

		var body struct {
			DelaySeconds int
			Reason       string
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.DelaySeconds != 30 {
			t.Errorf("expected delaySeconds 30, got %d", body.DelaySeconds)
		}
		if body.Reason != "temporary failure" {
			t.Errorf("expected reason 'temporary failure', got %s", body.Reason)
		}

		jsonResponse(w, http.StatusOK, NackResponse{
			Status:       "requeued",
			DelaySeconds: 30,
		})
	})

	resp, err := client.Nack(context.Background(), "task-abc", 30, "temporary failure")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "requeued" {
		t.Errorf("expected status 'requeued', got %s", resp.Status)
	}
}

func TestNack_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.Nack(context.Background(), "id", 30, "reason")
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// CreateSubscription
// ──────────────────────────────────────────────

func TestCreateSubscription(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/workers/subscriptions" {
			t.Errorf("expected /v1/codeq/workers/subscriptions, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusCreated, SubscriptionResponse{
			SubscriptionID: "sub-123",
			ExpiresAt:      "2026-01-01T00:00:00Z",
		})
	})

	resp, err := client.CreateSubscription(context.Background(), &CreateSubscriptionOptions{
		CallbackURL:  "https://example.com/webhook",
		EventTypes:   []string{"GENERATE_MASTER"},
		TTLSeconds:   Int(3600),
		DeliveryMode: "group",
		GroupID:      "pool-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SubscriptionID != "sub-123" {
		t.Errorf("expected subscription ID 'sub-123', got %s", resp.SubscriptionID)
	}
}

func TestCreateSubscription_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.CreateSubscription(context.Background(), &CreateSubscriptionOptions{
		CallbackURL: "https://example.com/webhook",
	})
	if err == nil {
		t.Fatal("expected error for missing worker token")
	}
}

// ──────────────────────────────────────────────
// RenewSubscription
// ──────────────────────────────────────────────

func TestRenewSubscription(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/workers/subscriptions/sub-123/heartbeat" {
			t.Errorf("expected /v1/codeq/workers/subscriptions/sub-123/heartbeat, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, SubscriptionResponse{
			SubscriptionID: "sub-123",
			ExpiresAt:      "2026-01-02T00:00:00Z",
		})
	})

	resp, err := client.RenewSubscription(context.Background(), "sub-123", &RenewSubscriptionOptions{
		TTLSeconds: Int(7200),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SubscriptionID != "sub-123" {
		t.Errorf("expected subscription ID 'sub-123', got %s", resp.SubscriptionID)
	}
}

func TestRenewSubscription_NilOptions(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, SubscriptionResponse{
			SubscriptionID: "sub-123",
			ExpiresAt:      "2026-01-02T00:00:00Z",
		})
	})

	resp, err := client.RenewSubscription(context.Background(), "sub-123", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SubscriptionID != "sub-123" {
		t.Errorf("expected subscription ID 'sub-123', got %s", resp.SubscriptionID)
	}
}

func TestRenewSubscription_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.RenewSubscription(context.Background(), "sub-123", nil)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

// ──────────────────────────────────────────────
// GetTask
// ──────────────────────────────────────────────

func TestGetTask(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/codeq/tasks/task-xyz" {
			t.Errorf("expected /v1/codeq/tasks/task-xyz, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, Task{
			ID:      "task-xyz",
			Command: "CMD",
			Status:  StatusCompleted,
		})
	})

	task, err := client.GetTask(context.Background(), "task-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "task-xyz" {
		t.Errorf("expected task ID 'task-xyz', got %s", task.ID)
	}
}

func TestGetTask_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.GetTask(context.Background(), "id")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

// ──────────────────────────────────────────────
// GetResult
// ──────────────────────────────────────────────

func TestGetResult(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/tasks/task-xyz/result" {
			t.Errorf("expected /v1/codeq/tasks/task-xyz/result, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, TaskResult{
			Task:   Task{ID: "task-xyz", Status: StatusCompleted},
			Result: ResultRecord{TaskID: "task-xyz", Status: StatusCompleted},
		})
	})

	result, err := client.GetResult(context.Background(), "task-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Task.ID != "task-xyz" {
		t.Errorf("expected task ID 'task-xyz', got %s", result.Task.ID)
	}
}

func TestGetResult_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.GetResult(context.Background(), "id")
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

// ──────────────────────────────────────────────
// WaitForResult
// ──────────────────────────────────────────────

func TestWaitForResult(t *testing.T) {
	calls := 0
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("not found"))
			return
		}
		jsonResponse(w, http.StatusOK, TaskResult{
			Task:   Task{ID: "task-xyz", Status: StatusCompleted},
			Result: ResultRecord{TaskID: "task-xyz", Status: StatusCompleted},
		})
	})

	result, err := client.WaitForResult(context.Background(), "task-xyz", &WaitForResultOptions{
		Timeout:      5 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Task.ID != "task-xyz" {
		t.Errorf("expected task ID 'task-xyz', got %s", result.Task.ID)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 calls, got %d", calls)
	}
}

func TestWaitForResult_Timeout(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	})

	_, err := client.WaitForResult(context.Background(), "task-xyz", &WaitForResultOptions{
		Timeout:      300 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForResult_ContextCancelled(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.WaitForResult(ctx, "task-xyz", &WaitForResultOptions{
		Timeout:      5 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
}

// ──────────────────────────────────────────────
// ListQueues
// ──────────────────────────────────────────────

func TestListQueues(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/queues" {
			t.Errorf("expected /v1/codeq/admin/queues, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Errorf("expected admin token")
		}

		jsonResponse(w, http.StatusOK, []QueueStats{
			{Command: "CMD1", Ready: 10, Delayed: 2, InProgress: 5, DLQ: 1},
		})
	})

	stats, err := client.ListQueues(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 queue stat, got %d", len(stats))
	}
	if stats[0].Ready != 10 {
		t.Errorf("expected 10 ready, got %d", stats[0].Ready)
	}
}

func TestListQueues_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.ListQueues(context.Background())
	if err == nil {
		t.Fatal("expected error for missing admin token")
	}
}

// ──────────────────────────────────────────────
// GetQueueStats
// ──────────────────────────────────────────────

func TestGetQueueStats(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/queues/GENERATE_MASTER" {
			t.Errorf("expected /v1/codeq/admin/queues/GENERATE_MASTER, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, QueueStats{
			Command:    "GENERATE_MASTER",
			Ready:      10,
			InProgress: 5,
		})
	})

	stats, err := client.GetQueueStats(context.Background(), "GENERATE_MASTER")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Command != "GENERATE_MASTER" {
		t.Errorf("expected command GENERATE_MASTER, got %s", stats.Command)
	}
}

func TestGetQueueStats_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.GetQueueStats(context.Background(), "CMD")
	if err == nil {
		t.Fatal("expected error for missing admin token")
	}
}

// ──────────────────────────────────────────────
// CleanupExpired
// ──────────────────────────────────────────────

func TestCleanupExpired(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/codeq/admin/tasks/cleanup" {
			t.Errorf("expected /v1/codeq/admin/tasks/cleanup, got %s", r.URL.Path)
		}

		jsonResponse(w, http.StatusOK, CleanupResult{
			Deleted: 42,
			Before:  "2026-01-01T00:00:00Z",
			Limit:   1000,
		})
	})

	result, err := client.CleanupExpired(context.Background(), &CleanupOptions{
		Limit: Int(500),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Deleted != 42 {
		t.Errorf("expected 42 deleted, got %d", result.Deleted)
	}
}

func TestCleanupExpired_NilOptions(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, CleanupResult{Deleted: 0, Before: "2026-01-01T00:00:00Z", Limit: 1000})
	})

	result, err := client.CleanupExpired(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", result.Deleted)
	}
}

func TestCleanupExpired_MissingToken(t *testing.T) {
	c := NewClient("http://localhost:8080")
	_, err := c.CleanupExpired(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for missing admin token")
	}
}

// ──────────────────────────────────────────────
// Error type
// ──────────────────────────────────────────────

func TestError_Error(t *testing.T) {
	err := &Error{StatusCode: 404, Message: "task not found"}
	expected := "codeq: 404 task not found"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

// ──────────────────────────────────────────────
// Int helper
// ──────────────────────────────────────────────

func TestInt(t *testing.T) {
	p := Int(42)
	if p == nil || *p != 42 {
		t.Errorf("expected pointer to 42")
	}
}

// ──────────────────────────────────────────────
// Token fallback behavior
// ──────────────────────────────────────────────

func TestGetTask_FallsBackToProducerToken(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer prod-token-only" {
			t.Errorf("expected producer token fallback, got %s", r.Header.Get("Authorization"))
		}
		jsonResponse(w, http.StatusOK, Task{ID: "t-1"})
	})
	client.workerToken = ""
	client.producerToken = "prod-token-only"

	_, err := client.GetTask(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListQueues_FallsBackToProducerToken(t *testing.T) {
	_, client := setupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer prod-token-only" {
			t.Errorf("expected producer token fallback, got %s", r.Header.Get("Authorization"))
		}
		jsonResponse(w, http.StatusOK, []QueueStats{})
	})
	client.adminToken = ""
	client.workerToken = ""
	client.producerToken = "prod-token-only"

	_, err := client.ListQueues(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
