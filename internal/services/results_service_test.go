package services

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
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
	repo := openPebbleStores(t).results
	uploader := &mockResultsUploader{}
	logger := slog.Default()
	now := func() time.Time { return time.Now() }

	svc := NewResultsService(repo, uploader, nil, logger, now, time.UTC)
	if svc == nil {
		t.Fatal("Expected service to be non-nil")
	}
}

func TestResultsServiceGetTaskNotFound(t *testing.T) {
	repo := openPebbleStores(t).results
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
	stores := openPebbleStores(t)
	taskRepo := stores.tasks
	task, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", "", time.Time{}, "")

	repo := stores.results
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
	stores := openPebbleStores(t)
	taskRepo := stores.tasks

	// Create 3 tasks
	task1, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data1"}`, 0, "", 5, "", "", time.Time{}, "")
	task2, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data2"}`, 0, "", 5, "", "", time.Time{}, "")
	task3, _ := taskRepo.Enqueue(context.Background(), domain.CmdGenerateMaster, `{"test":"data3"}`, 0, "", 5, "", "", time.Time{}, "")

	// Claim tasks to move them to in-progress
	cmds := []domain.Command{domain.CmdGenerateMaster}
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")
	_, _, _ = taskRepo.Claim(context.Background(), "worker1", cmds, 30, 1, 5, "")

	resultRepo := stores.results
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

func TestResultsServiceSubmit(t *testing.T) {
	const workerID = "submit-worker"
	stores := openPebbleStores(t)
	ctx := context.Background()
	_, err := stores.tasks.Enqueue(ctx, domain.CmdGenerateMaster, `{"k":"v"}`, 0, "https://example.com/hook", 5, "", "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, ok, err := stores.tasks.Claim(ctx, workerID, []domain.Command{domain.CmdGenerateMaster}, 30, 1, 5, "tenant-a")
	if err != nil || !ok || claimed == nil {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	svc := NewResultsService(stores.results, &mockResultsUploader{}, nil, slog.Default(), time.Now, time.UTC)

	if _, err := svc.Submit(ctx, "missing", domain.SubmitResultRequest{Status: domain.StatusCompleted, Result: map[string]any{"ok": true}}); err == nil || err.Error() != "task not found" {
		t.Fatalf("missing task: %v", err)
	}

	pending, err := stores.tasks.Enqueue(ctx, domain.CmdGenerateMaster, `{}`, 0, "", 5, "", "", time.Time{}, "tenant-a")
	if err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if _, err := svc.Submit(ctx, pending.ID, domain.SubmitResultRequest{Status: domain.StatusCompleted, Result: map[string]any{"ok": true}}); err == nil || err.Error() != "not-in-progress" {
		t.Fatalf("pending submit: %v", err)
	}
	if _, err := svc.Submit(ctx, claimed.ID, domain.SubmitResultRequest{WorkerID: "other", Status: domain.StatusCompleted, Result: map[string]any{"ok": true}}); err == nil || err.Error() != "not-owner" {
		t.Fatalf("wrong worker: %v", err)
	}
	if _, err := svc.Submit(ctx, claimed.ID, domain.SubmitResultRequest{WorkerID: workerID, Status: domain.StatusCompleted}); err == nil || err.Error() != "result required when status=COMPLETED" {
		t.Fatalf("missing result: %v", err)
	}
	if _, err := svc.Submit(ctx, claimed.ID, domain.SubmitResultRequest{WorkerID: workerID, Status: domain.StatusFailed}); err == nil || err.Error() != "error required when status=FAILED" {
		t.Fatalf("missing error: %v", err)
	}
	if _, err := svc.Submit(ctx, claimed.ID, domain.SubmitResultRequest{WorkerID: workerID, Status: "NOPE"}); err == nil || err.Error() != "invalid status" {
		t.Fatalf("invalid status: %v", err)
	}

	rec, err := svc.Submit(ctx, claimed.ID, domain.SubmitResultRequest{
		WorkerID: workerID,
		Status:   domain.StatusCompleted,
		Result:   map[string]any{"ok": true},
		Artifacts: []domain.ArtifactIn{
			{Name: "link", URL: "https://example.com/link"},
			{Name: "blob", ContentBase64: "aGVsbG8=", ContentType: "text/plain"},
			{Name: "skip"},
		},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if rec.Status != domain.StatusCompleted || len(rec.Artifacts) != 2 {
		t.Fatalf("record status=%s artifacts=%d", rec.Status, len(rec.Artifacts))
	}
	got, taskAfter, err := svc.Get(ctx, claimed.ID)
	if err != nil || got == nil || taskAfter.Status != domain.StatusCompleted {
		t.Fatalf("get after submit: rec=%v task=%v err=%v", got, taskAfter, err)
	}
}
