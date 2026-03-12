package bench

import (
	"encoding/json"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// BenchmarkJSONUnmarshalTask compares encoding/json vs sonic for task unmarshaling
func BenchmarkJSONUnmarshalTask(b *testing.B) {
	taskJSON := `{"id":"task-123","command":"process","payload":"test data","priority":5,"status":"pending","webhook":"https://example.com/hook","maxAttempts":3,"attempts":0,"visibleAt":"2025-03-12T20:37:31Z","createdAt":"2025-03-12T20:37:31Z","expiresAt":"2025-03-13T20:37:31Z","leaseExpiresAt":"0001-01-01T00:00:00Z","tenantID":"tenant-123"}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		_ = json.Unmarshal([]byte(taskJSON), &t)
	}
}

// BenchmarkSonicUnmarshalTask compares sonic for task unmarshaling
func BenchmarkSonicUnmarshalTask(b *testing.B) {
	taskJSON := `{"id":"task-123","command":"process","payload":"test data","priority":5,"status":"pending","webhook":"https://example.com/hook","maxAttempts":3,"attempts":0,"visibleAt":"2025-03-12T20:37:31Z","createdAt":"2025-03-12T20:37:31Z","expiresAt":"2025-03-13T20:37:31Z","leaseExpiresAt":"0001-01-01T00:00:00Z","tenantID":"tenant-123"}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		_ = sonic.Unmarshal([]byte(taskJSON), &t)
	}
}

// BenchmarkJSONMarshalTask compares encoding/json vs sonic for task marshaling
func BenchmarkJSONMarshalTask(b *testing.B) {
	task := domain.Task{
		ID:          "task-123",
		Command:     "process",
		Payload:     "test data",
		Priority:    5,
		Status:      domain.TaskStatusPending,
		Webhook:     "https://example.com/hook",
		MaxAttempts: 3,
		Attempts:    0,
		TenantID:    "tenant-123",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(task)
	}
}

// BenchmarkSonicMarshalTask compares sonic for task marshaling
func BenchmarkSonicMarshalTask(b *testing.B) {
	task := domain.Task{
		ID:          "task-123",
		Command:     "process",
		Payload:     "test data",
		Priority:    5,
		Status:      domain.TaskStatusPending,
		Webhook:     "https://example.com/hook",
		MaxAttempts: 3,
		Attempts:    0,
		TenantID:    "tenant-123",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(task)
	}
}

// BenchmarkJSONUnmarshalResult compares encoding/json vs sonic for result unmarshaling
func BenchmarkJSONUnmarshalResult(b *testing.B) {
	resultJSON := `{"taskID":"task-123","status":"completed","result":"success","errorMsg":"","completedAt":"2025-03-12T20:37:31Z"}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var r domain.ResultRecord
		_ = json.Unmarshal([]byte(resultJSON), &r)
	}
}

// BenchmarkSonicUnmarshalResult compares sonic for result unmarshaling
func BenchmarkSonicUnmarshalResult(b *testing.B) {
	resultJSON := `{"taskID":"task-123","status":"completed","result":"success","errorMsg":"","completedAt":"2025-03-12T20:37:31Z"}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var r domain.ResultRecord
		_ = sonic.Unmarshal([]byte(resultJSON), &r)
	}
}

// BenchmarkJSONMarshalResult compares encoding/json vs sonic for result marshaling
func BenchmarkJSONMarshalResult(b *testing.B) {
	result := domain.ResultRecord{
		TaskID:   "task-123",
		Status:   domain.TaskStatusCompleted,
		Result:   "success",
		ErrorMsg: "",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(result)
	}
}

// BenchmarkSonicMarshalResult compares sonic for result marshaling
func BenchmarkSonicMarshalResult(b *testing.B) {
	result := domain.ResultRecord{
		TaskID:   "task-123",
		Status:   domain.TaskStatusCompleted,
		Result:   "success",
		ErrorMsg: "",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sonic.Marshal(result)
	}
}
