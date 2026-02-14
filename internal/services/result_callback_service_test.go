package services

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestNewResultCallbackServiceDefaults(t *testing.T) {
	tests := []struct {
		name              string
		maxAttempts       int
		baseDelaySeconds  int
		maxDelaySeconds   int
	}{
		{"zero maxAttempts", 0, 2, 60},
		{"negative maxAttempts", -1, 2, 60},
		{"zero baseDelay", 5, 0, 60},
		{"zero maxDelay", 5, 2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewResultCallbackService(slog.Default(), "secret", tt.maxAttempts, tt.baseDelaySeconds, tt.maxDelaySeconds)
			if svc == nil {
				t.Fatal("Expected service to be non-nil")
			}
		})
	}
}

func TestResultCallbackServiceSendNoWebhook(t *testing.T) {
	svc := NewResultCallbackService(slog.Default(), "secret", 3, 1, 10)
	
	task := domain.Task{
		ID:      "task-123",
		Command: domain.CmdGenerateMaster,
		Webhook: "", // No webhook
	}
	
	result := map[string]any{"output": "success"}
	rec := domain.ResultRecord{
		TaskID:      "task-123",
		Status:      domain.StatusCompleted,
		Result:      result,
		CompletedAt: time.Now().UTC(),
	}
	
	// Should not panic or error
	svc.Send(context.Background(), task, rec)
}

func TestResultCallbackServiceSendWithWebhook(t *testing.T) {
	svc := NewResultCallbackService(slog.Default(), "secret", 3, 1, 10)
	
	task := domain.Task{
		ID:      "task-123",
		Command: domain.CmdGenerateMaster,
		Webhook: "https://example.com/webhook",
	}
	
	result := map[string]any{"output": "success"}
	rec := domain.ResultRecord{
		TaskID:      "task-123",
		Status:      domain.StatusCompleted,
		Result:      result,
		CompletedAt: time.Now().UTC(),
	}
	
	// Send is async (goroutine), so just verify it doesn't panic
	svc.Send(context.Background(), task, rec)
	
	// Give it a moment to start (though we can't verify success without a mock server)
	time.Sleep(100 * time.Millisecond)
}

func TestResultCallbackServiceBackoffDelay(t *testing.T) {
	svc := NewResultCallbackService(slog.Default(), "secret", 5, 2, 10).(*resultCallbackService)
	
	tests := []struct {
		name    string
		attempt int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"attempt 1", 1, 2 * time.Second, 2 * time.Second},
		{"attempt 2", 2, 4 * time.Second, 4 * time.Second},
		{"attempt 3", 3, 8 * time.Second, 8 * time.Second},
		{"attempt 4", 4, 10 * time.Second, 10 * time.Second}, // Capped at max
		{"attempt 5", 5, 10 * time.Second, 10 * time.Second}, // Capped at max
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.backoffDelay(tt.attempt)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("backoffDelay(%d) = %v, want between %v and %v", tt.attempt, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}
