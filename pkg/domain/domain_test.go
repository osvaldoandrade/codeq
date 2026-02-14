package domain

import (
	"testing"
)

func TestCommandMarshalBinary(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		want string
	}{
		{"generate master", CmdGenerateMaster, "GENERATE_MASTER"},
		{"generate creative", CmdGenerateCreative, "GENERATE_CREATIVE"},
		{"custom command", Command("CUSTOM_CMD"), "CUSTOM_CMD"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cmd.MarshalBinary()
			if err != nil {
				t.Errorf("MarshalBinary() error = %v", err)
				return
			}
			if string(got) != tt.want {
				t.Errorf("MarshalBinary() = %v, want %v", string(got), tt.want)
			}
		})
	}
}

func TestCommandMarshalText(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		want string
	}{
		{"generate master", CmdGenerateMaster, "GENERATE_MASTER"},
		{"generate creative", CmdGenerateCreative, "GENERATE_CREATIVE"},
		{"custom command", Command("CUSTOM_CMD"), "CUSTOM_CMD"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cmd.MarshalText()
			if err != nil {
				t.Errorf("MarshalText() error = %v", err)
				return
			}
			if string(got) != tt.want {
				t.Errorf("MarshalText() = %v, want %v", string(got), tt.want)
			}
		})
	}
}

func TestTaskStatusMarshalBinary(t *testing.T) {
	tests := []struct {
		name   string
		status TaskStatus
		want   string
	}{
		{"pending", StatusPending, "PENDING"},
		{"in progress", StatusInProgress, "IN_PROGRESS"},
		{"completed", StatusCompleted, "COMPLETED"},
		{"failed", StatusFailed, "FAILED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.status.MarshalBinary()
			if err != nil {
				t.Errorf("MarshalBinary() error = %v", err)
				return
			}
			if string(got) != tt.want {
				t.Errorf("MarshalBinary() = %v, want %v", string(got), tt.want)
			}
		})
	}
}

func TestTaskStatusMarshalText(t *testing.T) {
	tests := []struct {
		name   string
		status TaskStatus
		want   string
	}{
		{"pending", StatusPending, "PENDING"},
		{"in progress", StatusInProgress, "IN_PROGRESS"},
		{"completed", StatusCompleted, "COMPLETED"},
		{"failed", StatusFailed, "FAILED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.status.MarshalText()
			if err != nil {
				t.Errorf("MarshalText() error = %v", err)
				return
			}
			if string(got) != tt.want {
				t.Errorf("MarshalText() = %v, want %v", string(got), tt.want)
			}
		})
	}
}

func TestTaskFields(t *testing.T) {
	task := Task{
		ID:          "task-123",
		Command:     CmdGenerateMaster,
		Payload:     `{"test":"data"}`,
		Priority:    5,
		Webhook:     "https://example.com/webhook",
		Status:      StatusPending,
		WorkerID:    "worker-1",
		LeaseUntil:  "2024-01-01T12:00:00Z",
		Attempts:    1,
		MaxAttempts: 5,
		Error:       "",
		ResultKey:   "result-key",
	}

	if task.ID != "task-123" {
		t.Errorf("Expected ID 'task-123', got %s", task.ID)
	}
	if task.Command != CmdGenerateMaster {
		t.Errorf("Expected command %s, got %s", CmdGenerateMaster, task.Command)
	}
	if task.Priority != 5 {
		t.Errorf("Expected priority 5, got %d", task.Priority)
	}
	if task.Status != StatusPending {
		t.Errorf("Expected status %s, got %s", StatusPending, task.Status)
	}
}

func TestResultRecordFields(t *testing.T) {
	result := map[string]any{"output": "success"}
	rec := ResultRecord{
		TaskID: "task-123",
		Status: StatusCompleted,
		Result: result,
		Error:  "",
		Artifacts: []ArtifactOut{
			{Name: "output.txt", URL: "https://example.com/output.txt"},
		},
	}

	if rec.TaskID != "task-123" {
		t.Errorf("Expected TaskID 'task-123', got %s", rec.TaskID)
	}
	if rec.Status != StatusCompleted {
		t.Errorf("Expected status %s, got %s", StatusCompleted, rec.Status)
	}
	if len(rec.Artifacts) != 1 {
		t.Errorf("Expected 1 artifact, got %d", len(rec.Artifacts))
	}
}

func TestSubscriptionFields(t *testing.T) {
	sub := Subscription{
		ID:                 "sub-123",
		EventTypes:         []Command{CmdGenerateMaster},
		CallbackURL:        "https://example.com/callback",
		DeliveryMode:       "fanout",
		GroupID:            "group-1",
		MinIntervalSeconds: 30,
	}

	if sub.ID != "sub-123" {
		t.Errorf("Expected ID 'sub-123', got %s", sub.ID)
	}
	if len(sub.EventTypes) != 1 || sub.EventTypes[0] != CmdGenerateMaster {
		t.Errorf("Expected event type %s, got %v", CmdGenerateMaster, sub.EventTypes)
	}
	if sub.DeliveryMode != "fanout" {
		t.Errorf("Expected delivery mode 'fanout', got %s", sub.DeliveryMode)
	}
}

func TestQueueStatsFields(t *testing.T) {
	stats := QueueStats{
		Command:    CmdGenerateMaster,
		Ready:      10,
		InProgress: 15,
		Delayed:    3,
		DLQ:        2,
	}

	if stats.Command != CmdGenerateMaster {
		t.Errorf("Expected command %s, got %s", CmdGenerateMaster, stats.Command)
	}
	if stats.Ready != 10 {
		t.Errorf("Expected 10 ready, got %d", stats.Ready)
	}
	if stats.InProgress != 15 {
		t.Errorf("Expected 15 in progress, got %d", stats.InProgress)
	}
}

func TestArtifactInFields(t *testing.T) {
	artifact := ArtifactIn{
		Name:          "test.txt",
		URL:           "https://example.com/test.txt",
		ContentBase64: "dGVzdA==",
		ContentType:   "text/plain",
	}

	if artifact.Name != "test.txt" {
		t.Errorf("Expected name 'test.txt', got %s", artifact.Name)
	}
	if artifact.URL != "https://example.com/test.txt" {
		t.Errorf("Expected URL, got %s", artifact.URL)
	}
}

func TestArtifactOutFields(t *testing.T) {
	artifact := ArtifactOut{
		Name: "output.txt",
		URL:  "https://example.com/output.txt",
	}

	if artifact.Name != "output.txt" {
		t.Errorf("Expected name 'output.txt', got %s", artifact.Name)
	}
	if artifact.URL != "https://example.com/output.txt" {
		t.Errorf("Expected URL, got %s", artifact.URL)
	}
}

func TestSubmitResultRequestFields(t *testing.T) {
	result := map[string]any{"key": "value"}
	req := SubmitResultRequest{
		WorkerID: "worker-1",
		Status:   StatusCompleted,
		Result:   result,
		Error:    "",
		Artifacts: []ArtifactIn{
			{Name: "file.txt", ContentBase64: "data"},
		},
	}

	if req.WorkerID != "worker-1" {
		t.Errorf("Expected WorkerID 'worker-1', got %s", req.WorkerID)
	}
	if req.Status != StatusCompleted {
		t.Errorf("Expected status %s, got %s", StatusCompleted, req.Status)
	}
	if len(req.Artifacts) != 1 {
		t.Errorf("Expected 1 artifact, got %d", len(req.Artifacts))
	}
}
