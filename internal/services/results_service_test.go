package services

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

// mockUploader for testing
type mockResultsUploader struct {
	shouldFail bool
}

func (m *mockResultsUploader) UploadBytes(ctx context.Context, objPath string, contentType string, data []byte) (string, error) {
	if m.shouldFail {
		return "", &mockResultsError{"upload failed"}
	}
	return "https://example.com/" + objPath, nil
}

type mockResultsError struct {
	msg string
}

func (e *mockResultsError) Error() string {
	return e.msg
}

func TestNewResultsService(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	repo := repository.NewResultRepository(rdb, time.UTC, nil)
	uploader := &mockResultsUploader{}
	logger := slog.Default()
	now := func() time.Time { return time.Now() }

	svc := NewResultsService(repo, uploader, nil, logger, now, time.UTC)
	if svc == nil {
		t.Fatal("Expected service to be non-nil")
	}
}

func TestResultsServiceGetTaskNotFound(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	repo := repository.NewResultRepository(rdb, time.UTC, nil)
	uploader := &mockResultsUploader{}
	logger := slog.Default()
	now := func() time.Time { return time.Now() }

	svc := NewResultsService(repo, uploader, nil, logger, now, time.UTC)

	_, _, err := svc.Get(context.Background(), "nonexistent-task")
	if err == nil {
		t.Fatal("Expected error for nonexistent task")
	}
	if err.Error() != "task not found" {
		t.Errorf("Expected 'task not found', got %v", err)
	}
}

func TestResultsServiceGetResultNotFound(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// Create a task but no result
	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	task, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{}, "")

	repo := repository.NewResultRepository(rdb, time.UTC, nil)
	uploader := &mockResultsUploader{}
	logger := slog.Default()
	now := func() time.Time { return time.Now() }

	svc := NewResultsService(repo, uploader, nil, logger, now, time.UTC)

	_, _, err := svc.Get(context.Background(), task.ID)
	if err == nil {
		t.Fatal("Expected error for nonexistent result")
	}
	if err.Error() != "result not found" {
		t.Errorf("Expected 'result not found', got %v", err)
	}
}

func TestResultsServiceBatchSubmit(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// Setup task repository and create test tasks
	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)

	// Create 3 tasks
	task1, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data1"}`, 0, "", 5, "", time.Time{}, "")
	task2, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data2"}`, 0, "", 5, "", time.Time{}, "")
	task3, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data3"}`, 0, "", 5, "", time.Time{}, "")

	// Claim tasks to move them to in-progress
	cmds := []domain.Command{domain.CmdGenerateMaster}
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")

	resultRepo := repository.NewResultRepository(rdb, time.UTC, nil)
	uploader := &mockResultsUploader{}
	logger := slog.Default()
	now := func() time.Time { return time.Now() }

	svc := NewResultsService(resultRepo, uploader, nil, logger, now, time.UTC)

	// Prepare batch submit items
	items := []domain.BatchSubmitItem{
		{
			TaskID: task1.ID,
			SubmitResultRequest: domain.SubmitResultRequest{
				WorkerID: "worker1",
				Status:   domain.StatusCompleted,
				Result:   map[string]any{"output": "result1"},
			},
		},
		{
			TaskID: task2.ID,
			SubmitResultRequest: domain.SubmitResultRequest{
				WorkerID: "worker1",
				Status:   domain.StatusCompleted,
				Result:   map[string]any{"output": "result2"},
			},
		},
		{
			TaskID: task3.ID,
			SubmitResultRequest: domain.SubmitResultRequest{
				WorkerID: "worker1",
				Status:   domain.StatusFailed,
				Error:    "Task failed due to timeout",
			},
		},
	}

	// Execute batch submit
	responses, err := svc.BatchSubmit(context.Background(), items)
	if err != nil {
		t.Fatalf("BatchSubmit failed: %v", err)
	}

	// Validate responses
	if len(responses) != 3 {
		t.Errorf("Expected 3 responses, got %d", len(responses))
	}

	for i, resp := range responses {
		if resp.Error != "" {
			t.Errorf("Response %d has error: %s", i, resp.Error)
		}
		if resp.Result == nil && items[i].SubmitResultRequest.Status != domain.StatusFailed {
			t.Errorf("Response %d missing result", i)
		}
	}

	// Verify tasks were completed by checking they're no longer in-progress
	task, _ := taskRepo.Get(context.Background(), task1.ID)
	if task.Status != domain.StatusCompleted {
		t.Errorf("Expected task1 status to be COMPLETED, got %s", task.Status)
	}
}
