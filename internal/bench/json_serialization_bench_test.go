package bench

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Benchmark JSON marshaling performance: sonic vs encoding/json
// Tests the optimization of hot paths in task and result serialization

func BenchmarkJSONUnmarshalTask_StdLib(b *testing.B) {
	taskJSON := `{
		"id":"task-123",
		"command":"generate-master",
		"status":"pending",
		"priority":5,
		"payload":"test-payload",
		"webhook":"https://example.com/webhook",
		"maxAttempts":3,
		"idempotencyKey":"idempo-123",
		"visibleAt":"2024-01-01T00:00:00Z",
		"createdAt":"2024-01-01T00:00:00Z",
		"updatedAt":"2024-01-01T00:00:00Z",
		"leaseUntil":"2024-01-01T01:00:00Z",
		"tenantID":"tenant-1"
	}`
	data := []byte(taskJSON)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		_ = json.Unmarshal(data, &t)
	}
}

func BenchmarkJSONUnmarshalTask_Sonic(b *testing.B) {
	taskJSON := `{
		"id":"task-123",
		"command":"generate-master",
		"status":"pending",
		"priority":5,
		"payload":"test-payload",
		"webhook":"https://example.com/webhook",
		"maxAttempts":3,
		"idempotencyKey":"idempo-123",
		"visibleAt":"2024-01-01T00:00:00Z",
		"createdAt":"2024-01-01T00:00:00Z",
		"updatedAt":"2024-01-01T00:00:00Z",
		"leaseUntil":"2024-01-01T01:00:00Z",
		"tenantID":"tenant-1"
	}`
	data := []byte(taskJSON)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		_ = sonic.Unmarshal(data, &t)
	}
}

func BenchmarkJSONMarshalTask_StdLib(b *testing.B) {
	task := domain.Task{
		ID:              "task-123",
		Command:         domain.CmdGenerateMaster,
		Status:          domain.TaskStatusPending,
		Priority:        5,
		Payload:         "test-payload",
		Webhook:         "https://example.com/webhook",
		MaxAttempts:     3,
		IdempotencyKey:  "idempo-123",
		TenantID:        "tenant-1",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(task)
	}
}

func BenchmarkJSONMarshalTask_Sonic(b *testing.B) {
	task := domain.Task{
		ID:              "task-123",
		Command:         domain.CmdGenerateMaster,
		Status:          domain.TaskStatusPending,
		Priority:        5,
		Payload:         "test-payload",
		Webhook:         "https://example.com/webhook",
		MaxAttempts:     3,
		IdempotencyKey:  "idempo-123",
		TenantID:        "tenant-1",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(task)
	}
}

func BenchmarkJSONUnmarshalResultRecord_StdLib(b *testing.B) {
	resultJSON := `{
		"taskID":"task-123",
		"status":"completed",
		"output":"result-output",
		"createdAt":"2024-01-01T00:00:00Z"
	}`
	data := []byte(resultJSON)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var rec domain.ResultRecord
		_ = json.Unmarshal(data, &rec)
	}
}

func BenchmarkJSONUnmarshalResultRecord_Sonic(b *testing.B) {
	resultJSON := `{
		"taskID":"task-123",
		"status":"completed",
		"output":"result-output",
		"createdAt":"2024-01-01T00:00:00Z"
	}`
	data := []byte(resultJSON)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var rec domain.ResultRecord
		_ = sonic.Unmarshal(data, &rec)
	}
}

func BenchmarkJSONMarshalResultRecord_StdLib(b *testing.B) {
	rec := domain.ResultRecord{
		TaskID: "task-123",
		Status: domain.TaskStatusCompleted,
		Output: "result-output",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(rec)
	}
}

func BenchmarkJSONMarshalResultRecord_Sonic(b *testing.B) {
	rec := domain.ResultRecord{
		TaskID: "task-123",
		Status: domain.TaskStatusCompleted,
		Output: "result-output",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(rec)
	}
}
