package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/osvaldoandrade/codeq/internal/shard"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// isNotFoundErr checks whether an error represents a "not-found" condition.
// This maintains compatibility with the existing error convention used
// throughout the codebase (fmt.Errorf("not-found")).
func isNotFoundErr(err error) bool {
	return err != nil && err.Error() == "not-found"
}

// shardedTaskRepository distributes task operations across multiple Redis backends.
// Each shard gets its own taskRedisRepo instance, and operations are routed to the
// appropriate shard based on the ShardSupplier resolution.
type shardedTaskRepository struct {
	shardSupplier domain.ShardSupplier
	repos         map[string]TaskRepository
	defaultShard  string
}

// NewShardedTaskRepository creates a TaskRepository that routes operations across
// multiple Redis backends based on the ShardSupplier.
// Each shard in the clientMap gets its own underlying taskRedisRepo.
func NewShardedTaskRepository(
	clientMap *shard.ClientMap,
	tz *time.Location,
	backoffPolicy string,
	backoffBaseSeconds int,
	backoffMaxSeconds int,
	shardSupplier domain.ShardSupplier,
) TaskRepository {
	if shardSupplier == nil {
		shardSupplier = shard.NewDefaultShardSupplier()
	}

	shardIDs := clientMap.ShardIDs()
	repos := make(map[string]TaskRepository, len(shardIDs))
	for _, sid := range shardIDs {
		repos[sid] = NewTaskRepository(
			clientMap.Client(sid),
			tz,
			backoffPolicy,
			backoffBaseSeconds,
			backoffMaxSeconds,
			shardSupplier,
		)
	}

	return &shardedTaskRepository{
		shardSupplier: shardSupplier,
		repos:         repos,
		defaultShard:  clientMap.DefaultShard(),
	}
}

func (s *shardedTaskRepository) resolveShard(ctx context.Context, cmd domain.Command, tenantID string) string {
	sid, err := s.shardSupplier.CurrentShard(ctx, string(cmd), tenantID)
	if err != nil || sid == "" {
		return s.defaultShard
	}
	return sid
}

func (s *shardedTaskRepository) repoForShard(shardID string) TaskRepository {
	if repo, ok := s.repos[shardID]; ok {
		return repo
	}
	return s.repos[s.defaultShard]
}

func (s *shardedTaskRepository) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	sid := s.resolveShard(ctx, cmd, tenantID)
	return s.repoForShard(sid).Enqueue(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
}

func (s *shardedTaskRepository) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	// Try claiming from each command's resolved shard
	for _, cmd := range commands {
		sid := s.resolveShard(ctx, cmd, tenantID)
		task, ok, err := s.repoForShard(sid).Claim(ctx, workerID, []domain.Command{cmd}, leaseSeconds, inspectLimit, maxAttemptsDefault, tenantID)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return task, true, nil
		}
	}
	return nil, false, nil
}

func (s *shardedTaskRepository) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	// Fan out to all shards since we can't determine shard from taskID alone
	for _, repo := range s.repos {
		err := repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
		if err == nil {
			return nil
		}
		if !isNotFoundErr(err) {
			return err
		}
	}
	return fmt.Errorf("not-found")
}

func (s *shardedTaskRepository) Abandon(ctx context.Context, taskID string, workerID string) error {
	for _, repo := range s.repos {
		err := repo.Abandon(ctx, taskID, workerID)
		if err == nil {
			return nil
		}
		if !isNotFoundErr(err) {
			return err
		}
	}
	return fmt.Errorf("not-found")
}

func (s *shardedTaskRepository) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	for _, repo := range s.repos {
		delay, dlq, err := repo.Nack(ctx, taskID, workerID, delaySeconds, maxAttemptsDefault, reason)
		if err == nil {
			return delay, dlq, nil
		}
		if !isNotFoundErr(err) {
			return 0, false, err
		}
	}
	return 0, false, fmt.Errorf("not-found")
}

func (s *shardedTaskRepository) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	sid := s.resolveShard(ctx, cmd, "")
	return s.repoForShard(sid).MoveDueDelayed(ctx, cmd, limit)
}

func (s *shardedTaskRepository) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	// Aggregate pending length across all shards where this command may exist
	shards, err := s.shardSupplier.QueueShards(ctx, string(cmd), "")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, sid := range shards {
		n, err := s.repoForShard(sid).PendingLength(ctx, cmd)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (s *shardedTaskRepository) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	for _, repo := range s.repos {
		task, err := repo.Get(ctx, taskID)
		if err == nil {
			return task, nil
		}
		if !isNotFoundErr(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("not-found")
}

func (s *shardedTaskRepository) AdminQueues(ctx context.Context) (map[string]any, error) {
	merged := map[string]any{}
	for _, repo := range s.repos {
		result, err := repo.AdminQueues(ctx)
		if err != nil {
			return nil, err
		}
		for k, v := range result {
			if existing, ok := merged[k]; ok {
				// Sum int64 values from different shards
				if ev, ok := existing.(int64); ok {
					if nv, ok := v.(int64); ok {
						merged[k] = ev + nv
						continue
					}
				}
			}
			merged[k] = v
		}
	}
	return merged, nil
}

func (s *shardedTaskRepository) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	shards, err := s.shardSupplier.QueueShards(ctx, string(cmd), "")
	if err != nil {
		return nil, err
	}

	aggregate := &domain.QueueStats{Command: cmd}
	for _, sid := range shards {
		stats, err := s.repoForShard(sid).QueueStats(ctx, cmd)
		if err != nil {
			return nil, err
		}
		aggregate.Ready += stats.Ready
		aggregate.Delayed += stats.Delayed
		aggregate.InProgress += stats.InProgress
		aggregate.DLQ += stats.DLQ
	}
	return aggregate, nil
}

func (s *shardedTaskRepository) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	totalDeleted := 0
	perShardLimit := limit / len(s.repos)
	if perShardLimit <= 0 {
		perShardLimit = 1
	}
	for _, repo := range s.repos {
		deleted, err := repo.CleanupExpired(ctx, perShardLimit, before)
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += deleted
	}
	return totalDeleted, nil
}
