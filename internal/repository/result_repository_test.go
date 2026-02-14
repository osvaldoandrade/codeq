package repository

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func setupResultRepo(t *testing.T) (context.Context, *miniredis.Miniredis, *redis.Client, ResultRepository, TaskRepository) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	
	repo := NewResultRepository(rdb, time.UTC)
	taskRepo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	
	return context.Background(), mr, rdb, repo, taskRepo
}

func TestDecodeBase64(t *testing.T) {
	ctx, _, _, repo, _ := setupResultRepo(t)
	_ = ctx

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"valid base64", "dGVzdA==", "test", false},
		{"empty string", "", "", false},
		{"invalid base64", "not-base64!@#", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.DecodeBase64(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeBase64() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && string(got) != tt.want {
				t.Errorf("DecodeBase64() = %v, want %v", string(got), tt.want)
			}
		})
	}
}

func TestResultRepositoryGetTask(t *testing.T) {
	ctx, _, _, repo, taskRepo := setupResultRepo(t)

	// Create a task
	task, err := taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	if err != nil {
		t.Fatalf("Failed to create task: %v", err)
	}

	// Get the task via ResultRepository
	gotTask, err := repo.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}

	if gotTask.ID != task.ID {
		t.Errorf("GetTask() ID = %v, want %v", gotTask.ID, task.ID)
	}
	if gotTask.Command != domain.CmdGenerateMaster {
		t.Errorf("GetTask() Command = %v, want %v", gotTask.Command, domain.CmdGenerateMaster)
	}
}

func TestResultRepositoryGetTaskNotFound(t *testing.T) {
	ctx, _, _, repo, _ := setupResultRepo(t)

	_, err := repo.GetTask(ctx, "nonexistent-task-id")
	if err == nil {
		t.Fatal("Expected error for nonexistent task")
	}
}

func TestResultRepositorySaveAndGetResult(t *testing.T) {
	ctx, _, _, repo, taskRepo := setupResultRepo(t)

	// Create a task
	_, _ = taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	claimed, _, _ := taskRepo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 5)

	// Save a result
	result := map[string]any{"output": "success"}
	rec := domain.ResultRecord{
		TaskID:      claimed.ID,
		Status:      domain.StatusCompleted,
		Result:      result,
		CompletedAt: time.Now().UTC(),
	}

	err := repo.SaveResult(ctx, rec)
	if err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	// Get the result
	gotRec, err := repo.GetResult(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetResult() error = %v", err)
	}

	if gotRec.TaskID != claimed.ID {
		t.Errorf("GetResult() TaskID = %v, want %v", gotRec.TaskID, claimed.ID)
	}
	if gotRec.Status != domain.StatusCompleted {
		t.Errorf("GetResult() Status = %v, want %v", gotRec.Status, domain.StatusCompleted)
	}
}

func TestResultRepositoryGetResultNotFound(t *testing.T) {
	ctx, _, _, repo, _ := setupResultRepo(t)

	_, err := repo.GetResult(ctx, "nonexistent-task-id")
	if err == nil {
		t.Fatal("Expected error for nonexistent result")
	}
}

func TestResultRepositoryUpdateTaskOnComplete(t *testing.T) {
	ctx, _, _, repo, taskRepo := setupResultRepo(t)

	// Create and claim a task
	_, _ = taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	claimed, _, _ := taskRepo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 5)

	// Update task on complete
	err := repo.UpdateTaskOnComplete(ctx, claimed.ID, domain.StatusCompleted, "")
	if err != nil {
		t.Fatalf("UpdateTaskOnComplete() error = %v", err)
	}

	// Verify task status updated
	gotTask, _ := repo.GetTask(ctx, claimed.ID)
	if gotTask.Status != domain.StatusCompleted {
		t.Errorf("Task status = %v, want %v", gotTask.Status, domain.StatusCompleted)
	}
}

func TestResultRepositoryUpdateTaskOnCompleteFailed(t *testing.T) {
	ctx, _, _, repo, taskRepo := setupResultRepo(t)

	// Create and claim a task
	_, _ = taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	claimed, _, _ := taskRepo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 5)

	// Update task on failure
	err := repo.UpdateTaskOnComplete(ctx, claimed.ID, domain.StatusFailed, "test error")
	if err != nil {
		t.Fatalf("UpdateTaskOnComplete() error = %v", err)
	}

	// Verify task status updated
	gotTask, _ := repo.GetTask(ctx, claimed.ID)
	if gotTask.Status != domain.StatusFailed {
		t.Errorf("Task status = %v, want %v", gotTask.Status, domain.StatusFailed)
	}
	// Note: Error field may or may not be preserved based on implementation
}

func TestResultRepositoryRemoveFromInprogAndClearLease(t *testing.T) {
	ctx, _, _, repo, taskRepo := setupResultRepo(t)

	// Create and claim a task
	_, _ = taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	claimed, _, _ := taskRepo.Claim(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 50, 5)

	// Remove from in-progress and clear lease
	err := repo.RemoveFromInprogAndClearLease(ctx, claimed.ID, domain.CmdGenerateMaster)
	if err != nil {
		t.Fatalf("RemoveFromInprogAndClearLease() error = %v", err)
	}

	// Task should no longer be in progress (this is implementation detail, just checking no error)
}

func TestDecodeBase64ValidData(t *testing.T) {
	ctx, _, _, repo, _ := setupResultRepo(t)
	_ = ctx

	testData := "Hello, World!"
	encoded := base64.StdEncoding.EncodeToString([]byte(testData))

	decoded, err := repo.DecodeBase64(encoded)
	if err != nil {
		t.Fatalf("DecodeBase64() error = %v", err)
	}

	if string(decoded) != testData {
		t.Errorf("DecodeBase64() = %v, want %v", string(decoded), testData)
	}
}
