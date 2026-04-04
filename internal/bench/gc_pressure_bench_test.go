package bench

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// BenchmarkGCPressure_SustainedEnqueue measures allocation pressure during a
// sustained stream of CreateTask calls.  The Go benchmark framework reports
// B/op and allocs/op; a regression in either number signals increased GC load.
func BenchmarkGCPressure_SustainedEnqueue(b *testing.B) {
	a := newBenchApp(b)
	ctx := context.Background()

	// Warm up: let the runtime settle.
	for i := 0; i < 50; i++ {
		_, _ = a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"warmup":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)
	}
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"gc":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)
		if err != nil {
			b.Fatalf("CreateTask: %v", err)
		}
	}
	b.StopTimer()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	b.ReportMetric(float64(after.NumGC-before.NumGC), "gc_cycles")
	b.ReportMetric(float64(after.PauseTotalNs-before.PauseTotalNs)/1e6, "gc_pause_ms")
}

// BenchmarkGCPressure_ClaimSubmitCycle measures allocation pressure during the
// full claim→submit cycle which is the hottest worker-side path.
func BenchmarkGCPressure_ClaimSubmitCycle(b *testing.B) {
	a := newBenchApp(b)
	ctx := context.Background()

	// Keep queue filled so claims always succeed.
	const prefill = 500
	for i := 0; i < prefill; i++ {
		_, err := a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"pre":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)
		if err != nil {
			b.Fatalf("prefill CreateTask: %v", err)
		}
	}
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Replenish queue to keep depth > 0.
		_, _ = a.Scheduler.CreateTask(ctx, domain.CmdGenerateMaster, `{"gc":true}`, 0, "", 0, "", time.Time{}, 0, benchTenant)

		task, ok, err := a.Scheduler.ClaimTask(ctx, benchWorkerSub, []domain.Command{domain.CmdGenerateMaster}, 60, 0, benchTenant)
		if err != nil || !ok || task == nil {
			b.Fatalf("ClaimTask: ok=%v err=%v", ok, err)
		}

		_, err = a.Results.Submit(ctx, task.ID, domain.SubmitResultRequest{
			WorkerID: benchWorkerSub,
			Status:   domain.StatusCompleted,
			Result:   map[string]any{"ok": true},
		})
		if err != nil {
			b.Fatalf("Submit: %v", err)
		}
	}
	b.StopTimer()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	b.ReportMetric(float64(after.NumGC-before.NumGC), "gc_cycles")
	b.ReportMetric(float64(after.PauseTotalNs-before.PauseTotalNs)/1e6, "gc_pause_ms")
}
