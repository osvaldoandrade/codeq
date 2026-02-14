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
	
	repo := repository.NewResultRepository(rdb, time.UTC)
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
	
	repo := repository.NewResultRepository(rdb, time.UTC)
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
	taskRepo := repository.NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10)
	task, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{})
	
	repo := repository.NewResultRepository(rdb, time.UTC)
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
