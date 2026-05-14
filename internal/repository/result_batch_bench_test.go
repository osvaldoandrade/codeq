package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

// BenchmarkBatchSaveResults_Sequential_vs_Batch compares sequential SaveResult() calls
// vs the new BatchSaveResults() implementation.
//
// Expected performance improvement:
// - Sequential (N independent RTTs): ~2ms per item for batch of 100 = 200ms total
// - Batch (2 RTTs): ~2 RTTs = ~4ms total
// - Overall: 50x improvement (from 200ms to 4ms for batch-100)
func BenchmarkBatchSaveResults(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis start: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	resultRepo := NewResultRepository(rdb, time.UTC)
	taskRepo := NewTaskRepository(rdb, time.UTC, "exp_full_jitter", 1, 10, nil)
	ctx := context.Background()

	// Benchmark different batch sizes
	for _, batchSize := range []int{10, 50, 100} {
		b.Run(fmt.Sprintf("BatchSaveResults_size_%d", batchSize), func(b *testing.B) {
			// Setup: Create tasks and result records
			resultRecords := make([]domain.ResultRecord, batchSize)
			for i := 0; i < batchSize; i++ {
				// Create a task
				task, err := taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{}, "")
				if err != nil {
					b.Fatalf("enqueue task: %v", err)
				}

				// Create a result record for this task
				resultRecords[i] = domain.ResultRecord{
					TaskID:      task.ID,
					Status:      domain.StatusCompleted,
					Result:      map[string]any{"output": "test"},
					Error:       "",
					Artifacts:   []domain.ArtifactOut{},
					CompletedAt: time.Now(),
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := resultRepo.BatchSaveResults(ctx, resultRecords)
				if err != nil {
					b.Fatalf("batch save results: %v", err)
				}
			}
		})
	}

	// Benchmark sequential SaveResult for comparison
	b.Run("Sequential_SaveResult_size_10", func(b *testing.B) {
		resultRecords := make([]domain.ResultRecord, 10)
		for i := 0; i < 10; i++ {
			task, err := taskRepo.Enqueue(ctx, domain.CmdGenerateMaster, `{"test":"data"}`, 0, "", 5, "", time.Time{}, "")
			if err != nil {
				b.Fatalf("enqueue task: %v", err)
			}
			resultRecords[i] = domain.ResultRecord{
				TaskID:      task.ID,
				Status:      domain.StatusCompleted,
				Result:      map[string]any{"output": "test"},
				Error:       "",
				Artifacts:   []domain.ArtifactOut{},
				CompletedAt: time.Now(),
			}
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, rec := range resultRecords {
				err := resultRepo.SaveResult(ctx, rec)
				if err != nil {
					b.Fatalf("save result: %v", err)
				}
			}
		}
	})
}
