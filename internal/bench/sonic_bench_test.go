package bench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// sampleTask returns a domain.Task whose Payload field contains roughly
// payloadSize bytes of JSON.  Three sizes are used to exercise the codec
// across typical codeQ workloads.
func sampleTask(payloadSize int) domain.Task {
	return domain.Task{
		ID:          "bench-task-00000000-0000-0000-0000-000000000001",
		Command:     domain.CmdGenerateMaster,
		Payload:     `{"data":"` + strings.Repeat("x", payloadSize) + `"}`,
		Priority:    5,
		Status:      domain.StatusPending,
		WorkerID:    "worker-1",
		LeaseUntil:  "2026-01-01T00:00:00Z",
		Attempts:    1,
		MaxAttempts: 5,
		TenantID:    "tenant-bench",
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// ---------- Marshal benchmarks ----------

func benchSonicMarshal(b *testing.B, task domain.Task) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := sonic.Marshal(task)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchStdMarshal(b *testing.B, task domain.Task) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := json.Marshal(task)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSonic_MarshalTask_Small(b *testing.B)  { benchSonicMarshal(b, sampleTask(64)) }
func BenchmarkSonic_MarshalTask_Medium(b *testing.B) { benchSonicMarshal(b, sampleTask(1024)) }
func BenchmarkSonic_MarshalTask_Large(b *testing.B)  { benchSonicMarshal(b, sampleTask(8192)) }
func BenchmarkStdJSON_MarshalTask_Small(b *testing.B)  { benchStdMarshal(b, sampleTask(64)) }
func BenchmarkStdJSON_MarshalTask_Medium(b *testing.B) { benchStdMarshal(b, sampleTask(1024)) }
func BenchmarkStdJSON_MarshalTask_Large(b *testing.B)  { benchStdMarshal(b, sampleTask(8192)) }

// ---------- Unmarshal benchmarks ----------

func benchSonicUnmarshal(b *testing.B, task domain.Task) {
	b.Helper()
	data, err := sonic.Marshal(task)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		if err := sonic.Unmarshal(data, &t); err != nil {
			b.Fatal(err)
		}
	}
}

func benchStdUnmarshal(b *testing.B, task domain.Task) {
	b.Helper()
	data, err := json.Marshal(task)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var t domain.Task
		if err := json.Unmarshal(data, &t); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSonic_UnmarshalTask_Small(b *testing.B)  { benchSonicUnmarshal(b, sampleTask(64)) }
func BenchmarkSonic_UnmarshalTask_Medium(b *testing.B) { benchSonicUnmarshal(b, sampleTask(1024)) }
func BenchmarkSonic_UnmarshalTask_Large(b *testing.B)  { benchSonicUnmarshal(b, sampleTask(8192)) }
func BenchmarkStdJSON_UnmarshalTask_Small(b *testing.B)  { benchStdUnmarshal(b, sampleTask(64)) }
func BenchmarkStdJSON_UnmarshalTask_Medium(b *testing.B) { benchStdUnmarshal(b, sampleTask(1024)) }
func BenchmarkStdJSON_UnmarshalTask_Large(b *testing.B)  { benchStdUnmarshal(b, sampleTask(8192)) }
