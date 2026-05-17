package pebble

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// ShardedResultRepository routes by task ID across N per-shard
// ResultRepository instances. Phase 8 wrapper that pairs with
// ShardedTaskRepository — task body lives on a specific shard, and
// every Get / Save / Update for it lands there too.
//
// Cross-shard operations:
//   - GetTasksBatch: groups IDs by shard, parallel fetches, merges
//   - BatchUpdateTasksOnComplete: groups updates by shard, parallel
//     commits (one Pebble batch per shard, all atomic within their shard)
//   - BatchRemoveFromInprogAndClearLease: same grouping pattern
type ShardedResultRepository struct {
	shards []*ResultRepository
}

func NewShardedResultRepository(shards []*ResultRepository) *ShardedResultRepository {
	if len(shards) == 0 {
		panic("pebble: ShardedResultRepository requires at least one shard")
	}
	return &ShardedResultRepository{shards: shards}
}

func (s *ShardedResultRepository) shardOf(key string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum64() % uint64(len(s.shards)))
}

func (s *ShardedResultRepository) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	return s.shards[s.shardOf(id)].GetTask(ctx, id)
}

func (s *ShardedResultRepository) GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error) {
	return s.shards[s.shardOf(id)].GetTaskAndResult(ctx, id)
}

func (s *ShardedResultRepository) SaveResult(ctx context.Context, rec domain.ResultRecord, cmd domain.Command, tenantID string) error {
	return s.shards[s.shardOf(rec.TaskID)].SaveResult(ctx, rec, cmd, tenantID)
}

func (s *ShardedResultRepository) GetResult(ctx context.Context, id string) (*domain.ResultRecord, error) {
	return s.shards[s.shardOf(id)].GetResult(ctx, id)
}

func (s *ShardedResultRepository) UpdateTaskOnComplete(ctx context.Context, id string, cmd domain.Command, tenantID string, status domain.TaskStatus, errorMsg string) error {
	return s.shards[s.shardOf(id)].UpdateTaskOnComplete(ctx, id, cmd, tenantID, status, errorMsg)
}

func (s *ShardedResultRepository) RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command, tenantID string) error {
	return s.shards[s.shardOf(id)].RemoveFromInprogAndClearLease(ctx, id, cmd, tenantID)
}

func (s *ShardedResultRepository) DecodeBase64(str string) ([]byte, error) {
	if m := len(str) % 4; m != 0 {
		str += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(str)
}

// GetTasksBatch groups IDs by shard, fetches in parallel, merges.
func (s *ShardedResultRepository) GetTasksBatch(ctx context.Context, ids []string) (map[string]*domain.Task, error) {
	if len(ids) == 0 {
		return map[string]*domain.Task{}, nil
	}
	// Bucket IDs by shard.
	buckets := make(map[int][]string, len(s.shards))
	for _, id := range ids {
		idx := s.shardOf(id)
		buckets[idx] = append(buckets[idx], id)
	}
	out := make(map[string]*domain.Task, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for idx, b := range buckets {
		wg.Add(1)
		go func(shardIdx int, batch []string) {
			defer wg.Done()
			m, err := s.shards[shardIdx].GetTasksBatch(ctx, batch)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			mu.Lock()
			for k, v := range m {
				out[k] = v
			}
			mu.Unlock()
		}(idx, b)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// BatchUpdateTasksOnComplete groups updates by shard, commits each
// group as a single Pebble batch on its shard. All groups run in
// parallel because shards have independent commit pipelines.
func (s *ShardedResultRepository) BatchUpdateTasksOnComplete(ctx context.Context, updates []domain.TaskCompleteUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	buckets := make(map[int][]domain.TaskCompleteUpdate, len(s.shards))
	for _, u := range updates {
		idx := s.shardOf(u.ID)
		buckets[idx] = append(buckets[idx], u)
	}
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for idx, batch := range buckets {
		wg.Add(1)
		go func(shardIdx int, b []domain.TaskCompleteUpdate) {
			defer wg.Done()
			if err := s.shards[shardIdx].BatchUpdateTasksOnComplete(ctx, b); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}(idx, batch)
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("sharded batch update: %w", firstErr)
	}
	return nil
}

func (s *ShardedResultRepository) BatchRemoveFromInprogAndClearLease(ctx context.Context, deletes []domain.TaskDeleteInfo) error {
	if len(deletes) == 0 {
		return nil
	}
	buckets := make(map[int][]domain.TaskDeleteInfo, len(s.shards))
	for _, d := range deletes {
		idx := s.shardOf(d.ID)
		buckets[idx] = append(buckets[idx], d)
	}
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for idx, batch := range buckets {
		wg.Add(1)
		go func(shardIdx int, b []domain.TaskDeleteInfo) {
			defer wg.Done()
			if err := s.shards[shardIdx].BatchRemoveFromInprogAndClearLease(ctx, b); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}(idx, batch)
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("sharded batch remove: %w", firstErr)
	}
	return nil
}

// Compile-time check.
var _ repository.ResultRepository = (*ShardedResultRepository)(nil)

// Unused import guard while the file evolves. `time` is referenced
// once we add the AdminQueues-style reaper helpers; keep the import
// alive until then so iteration stays smooth.
var _ = time.Now
