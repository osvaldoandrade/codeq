package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

// --- mock services ---

type mockSchedulerService struct {
	createFunc func(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error)
	claimFunc  func(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, waitSeconds int, tenantID string) (*domain.Task, bool, error)
}

func (m *mockSchedulerService) CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error) {
	if m.createFunc != nil {
		return m.createFunc(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, runAt, delaySeconds, tenantID)
	}
	return &domain.Task{ID: "task-1", Command: cmd, Status: domain.StatusPending}, nil
}

func (m *mockSchedulerService) ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, waitSeconds int, tenantID string) (*domain.Task, bool, error) {
	if m.claimFunc != nil {
		return m.claimFunc(ctx, workerID, commands, leaseSeconds, waitSeconds, tenantID)
	}
	return &domain.Task{ID: "task-1", Status: domain.StatusInProgress}, true, nil
}

func (m *mockSchedulerService) Heartbeat(context.Context, string, string, int) error {
	return nil
}
func (m *mockSchedulerService) Abandon(context.Context, string, string) error { return nil }
func (m *mockSchedulerService) NackTask(context.Context, string, string, int, string) (int, bool, error) {
	return 0, false, nil
}
func (m *mockSchedulerService) GetTask(context.Context, string) (*domain.Task, error) {
	return nil, nil
}
func (m *mockSchedulerService) AdminQueues(context.Context) (map[string]any, error) {
	return nil, nil
}
func (m *mockSchedulerService) QueueStats(context.Context, domain.Command) (*domain.QueueStats, error) {
	return nil, nil
}
func (m *mockSchedulerService) CleanupExpired(context.Context, int, time.Time) (int, error) {
	return 0, nil
}

type mockResultsService struct {
	submitFunc func(ctx context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error)
}

func (m *mockResultsService) Submit(ctx context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error) {
	if m.submitFunc != nil {
		return m.submitFunc(ctx, taskID, req)
	}
	return &domain.ResultRecord{TaskID: taskID, Status: req.Status}, nil
}

func (m *mockResultsService) Get(context.Context, string) (*domain.ResultRecord, *domain.Task, error) {
	return nil, nil, nil
}

// --- helpers ---

func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewBuffer(b)
}

func newTestContext(t *testing.T, body *bytes.Buffer) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", body)
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx, rec
}

func setWorkerClaims(c *gin.Context, subject string, eventTypes []string) {
	c.Set("workerClaims", &auth.Claims{
		Subject:    subject,
		EventTypes: eventTypes,
	})
}

// --- Batch Create Tests ---

func TestBatchCreateTask_Success(t *testing.T) {
	callCount := 0
	svc := &mockSchedulerService{
		createFunc: func(_ context.Context, cmd domain.Command, _ string, _ int, _ string, _ int, _ string, _ time.Time, _ int, _ string) (*domain.Task, error) {
			callCount++
			return &domain.Task{ID: fmt.Sprintf("task-%d", callCount), Command: cmd, Status: domain.StatusPending}, nil
		},
	}
	ctrl := NewBatchCreateTaskController(svc)

	body := jsonBody(t, map[string]any{
		"tasks": []map[string]any{
			{"command": "GENERATE_MASTER", "payload": map[string]string{"k": "v1"}},
			{"command": "GENERATE_CREATIVE", "payload": map[string]string{"k": "v2"}},
		},
	})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []batchCreateResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Task == nil || resp.Results[0].Error != "" {
		t.Errorf("first result should succeed: %+v", resp.Results[0])
	}
	if resp.Results[1].Task == nil || resp.Results[1].Error != "" {
		t.Errorf("second result should succeed: %+v", resp.Results[1])
	}
	if callCount != 2 {
		t.Errorf("expected 2 service calls, got %d", callCount)
	}
}

func TestBatchCreateTask_PartialFailure(t *testing.T) {
	callCount := 0
	svc := &mockSchedulerService{
		createFunc: func(_ context.Context, _ domain.Command, _ string, _ int, _ string, _ int, _ string, _ time.Time, _ int, _ string) (*domain.Task, error) {
			callCount++
			if callCount == 2 {
				return nil, fmt.Errorf("invalid command")
			}
			return &domain.Task{ID: fmt.Sprintf("task-%d", callCount), Status: domain.StatusPending}, nil
		},
	}
	ctrl := NewBatchCreateTaskController(svc)

	body := jsonBody(t, map[string]any{
		"tasks": []map[string]any{
			{"command": "CMD1", "payload": "p1"},
			{"command": "", "payload": "p2"},
			{"command": "CMD3", "payload": "p3"},
		},
	})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []batchCreateResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Task == nil {
		t.Error("first result should succeed")
	}
	if resp.Results[1].Error == "" {
		t.Error("second result should have error")
	}
	if resp.Results[2].Task == nil {
		t.Error("third result should succeed")
	}
}

func TestBatchCreateTask_ExceedsMax(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchCreateTaskController(svc)

	tasks := make([]map[string]any, 101)
	for i := range tasks {
		tasks[i] = map[string]any{"command": "CMD", "payload": "p"}
	}
	body := jsonBody(t, map[string]any{"tasks": tasks})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBatchCreateTask_EmptyBody(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchCreateTaskController(svc)

	body := jsonBody(t, map[string]any{})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBatchCreateTask_InvalidRunAt(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchCreateTaskController(svc)

	body := jsonBody(t, map[string]any{
		"tasks": []map[string]any{
			{"command": "CMD1", "payload": "p1", "runAt": "not-a-date"},
		},
	})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Results []batchCreateResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Results[0].Error == "" {
		t.Error("expected error for invalid runAt")
	}
}

// --- Batch Claim Tests ---

func TestBatchClaimTask_Success(t *testing.T) {
	callCount := 0
	svc := &mockSchedulerService{
		claimFunc: func(_ context.Context, _ string, _ []domain.Command, _ int, _ int, _ string) (*domain.Task, bool, error) {
			callCount++
			if callCount > 3 {
				return nil, false, nil
			}
			return &domain.Task{ID: fmt.Sprintf("task-%d", callCount), Status: domain.StatusInProgress}, true, nil
		},
	}
	ctrl := NewBatchClaimTaskController(svc)

	body := jsonBody(t, map[string]any{
		"commands": []string{"GENERATE_MASTER"},
		"count":    5,
	})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tasks []*domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Tasks) != 3 {
		t.Fatalf("expected 3 tasks (available), got %d", len(resp.Tasks))
	}
}

func TestBatchClaimTask_NoTasks(t *testing.T) {
	svc := &mockSchedulerService{
		claimFunc: func(_ context.Context, _ string, _ []domain.Command, _ int, _ int, _ string) (*domain.Task, bool, error) {
			return nil, false, nil
		},
	}
	ctrl := NewBatchClaimTaskController(svc)

	body := jsonBody(t, map[string]any{"count": 3})
	ctx, _ := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	// gin's c.Status() sets the writer status without flushing to the recorder
	if ctx.Writer.Status() != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", ctx.Writer.Status())
	}
}

func TestBatchClaimTask_ExceedsMax(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchClaimTaskController(svc)

	body := jsonBody(t, map[string]any{"count": 11})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBatchClaimTask_MissingClaims(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchClaimTaskController(svc)

	body := jsonBody(t, map[string]any{"count": 1})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestBatchClaimTask_ForbiddenCommand(t *testing.T) {
	svc := &mockSchedulerService{}
	ctrl := NewBatchClaimTaskController(svc)

	body := jsonBody(t, map[string]any{
		"commands": []string{"NOT_ALLOWED"},
		"count":    1,
	})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"GENERATE_MASTER"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// --- Batch Submit Result Tests ---

func TestBatchSubmitResult_Success(t *testing.T) {
	svc := &mockResultsService{
		submitFunc: func(_ context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error) {
			return &domain.ResultRecord{TaskID: taskID, Status: req.Status}, nil
		},
	}
	ctrl := NewBatchSubmitResultController(svc)

	body := jsonBody(t, map[string]any{
		"results": []map[string]any{
			{"taskId": "t1", "status": "COMPLETED", "result": map[string]string{"out": "ok"}},
			{"taskId": "t2", "status": "FAILED", "error": "something went wrong"},
		},
	})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []batchSubmitResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Error != "" || resp.Results[0].Result == nil {
		t.Errorf("first should succeed: %+v", resp.Results[0])
	}
	if resp.Results[1].Error != "" || resp.Results[1].Result == nil {
		t.Errorf("second should succeed: %+v", resp.Results[1])
	}
}

func TestBatchSubmitResult_PartialFailure(t *testing.T) {
	svc := &mockResultsService{
		submitFunc: func(_ context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error) {
			if taskID == "t2" {
				return nil, fmt.Errorf("task not found")
			}
			return &domain.ResultRecord{TaskID: taskID, Status: req.Status}, nil
		},
	}
	ctrl := NewBatchSubmitResultController(svc)

	body := jsonBody(t, map[string]any{
		"results": []map[string]any{
			{"taskId": "t1", "status": "COMPLETED", "result": map[string]string{"out": "ok"}},
			{"taskId": "t2", "status": "COMPLETED", "result": map[string]string{"out": "ok"}},
		},
	})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Results []batchSubmitResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Results[0].Error != "" || resp.Results[0].Result == nil {
		t.Error("first should succeed")
	}
	if resp.Results[1].Error == "" {
		t.Error("second should have error")
	}
}

func TestBatchSubmitResult_MissingClaims(t *testing.T) {
	svc := &mockResultsService{}
	ctrl := NewBatchSubmitResultController(svc)

	body := jsonBody(t, map[string]any{
		"results": []map[string]any{
			{"taskId": "t1", "status": "COMPLETED", "result": map[string]string{"out": "ok"}},
		},
	})
	ctx, rec := newTestContext(t, body)
	ctrl.Handle(ctx)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestBatchSubmitResult_ExceedsMax(t *testing.T) {
	svc := &mockResultsService{}
	ctrl := NewBatchSubmitResultController(svc)

	results := make([]map[string]any, 101)
	for i := range results {
		results[i] = map[string]any{"taskId": fmt.Sprintf("t%d", i), "status": "COMPLETED", "result": map[string]string{"k": "v"}}
	}
	body := jsonBody(t, map[string]any{"results": results})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBatchSubmitResult_EmptyBody(t *testing.T) {
	svc := &mockResultsService{}
	ctrl := NewBatchSubmitResultController(svc)

	body := jsonBody(t, map[string]any{})
	ctx, rec := newTestContext(t, body)
	setWorkerClaims(ctx, "worker-1", []string{"*"})
	ctrl.Handle(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// Verify that the mock satisfies the SchedulerService interface
var _ services.SchedulerService = (*mockSchedulerService)(nil)

// Verify that the mock satisfies the ResultsService interface
var _ services.ResultsService = (*mockResultsService)(nil)
