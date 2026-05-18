package pebble

import (
	"context"
	"errors"
	"hash/fnv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// isNotLeader checks whether err is the local pebble.ErrNotLeader
// sentinel. In raft mode, a shard's local writes return this when the
// current node isn't the leader for that shard's raft group; the
// fan-out methods below treat it as "skip this shard, try the next"
// rather than a fatal error.
func isNotLeader(err error) bool {
	return errors.Is(err, ErrNotLeader)
}

// ShardedTaskRepository owns N TaskRepository instances (one per Pebble
// shard) and routes every task-keyed operation by hash(task_id) % N.
// Phase 8 single-node parallelism: compaction, commit pipeline, and the
// commit coalescer all run independently per shard, so writes on one
// shard don't stall writes on another.
//
// Atomic invariant: all keys derived from a single task ID (KeyTask,
// KeyPending, KeyInprog, KeyTTLIndex, KeyDelayed, KeyDLQ) hash to the
// same shard because the routing key IS the task ID. Each individual
// task operation still gets a single Pebble batch with one commit.
//
// Cross-shard operations:
//   - Claim / ClaimMany: round-robin across shards each call; per-shard
//     channels give the fast-path drain
//   - MoveDueDelayed: fan-out per shard
//   - AdminQueues / PendingLength / QueueStats: aggregate across shards
//   - Idempotency: lookup goes to the shard hash(idempotency_key) % N;
//     the original Enqueue wrote the idempo map into the same shard, so
//     replays land back on the right entry
type ShardedTaskRepository struct {
	shards    []*TaskRepository
	rrCounter atomic.Uint64
}

// NewShardedTaskRepository builds the wrapper over a pre-constructed
// slice of per-shard TaskRepositories. Caller owns DB lifecycles.
func NewShardedTaskRepository(shards []*TaskRepository) *ShardedTaskRepository {
	if len(shards) == 0 {
		panic("pebble: ShardedTaskRepository requires at least one shard")
	}
	return &ShardedTaskRepository{shards: shards}
}

// shardOf returns the shard index for the given key (task ID or any
// stable string). FNV-1a 64-bit is cheap and uniform — task IDs are
// UUID v4 strings, so any decent hash gives near-uniform balance.
func (s *ShardedTaskRepository) shardOf(key string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum64() % uint64(len(s.shards)))
}

// nextStart returns the round-robin starting shard for fan-out scans.
// Wrapping uint64 atomic — no need to worry about overflow.
func (s *ShardedTaskRepository) nextStart() int {
	return int(s.rrCounter.Add(1) % uint64(len(s.shards)))
}

// ---------------- TaskRepository interface ----------------

func (s *ShardedTaskRepository) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	task, _, err := s.EnqueueWithReady(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
	return task, err
}

func (s *ShardedTaskRepository) EnqueueWithReady(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error) {
	// Idempotency check against its own shard first.
	if idempotencyKey != "" {
		idShard := s.shardOf(idempotencyKey)
		if existing, err := s.shards[idShard].db.Get(KeyIdempo(idempotencyKey)); err == nil {
			existingID := string(existing)
			tShard := s.shardOf(existingID)
			task, ferr := s.shards[tShard].Get(ctx, existingID)
			if ferr == nil {
				return task, false, nil
			}
		}
	}
	// Pick an ID and dispatch to its owning shard.
	id := uuid.NewString()
	tShard := s.shardOf(id)
	return s.shards[tShard].EnqueueWithID(ctx, id, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
}

func (s *ShardedTaskRepository) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	start := s.nextStart()
	n := len(s.shards)
	for i := range n {
		idx := (start + i) % n
		t, ok, err := s.shards[idx].Claim(ctx, workerID, commands, leaseSeconds, inspectLimit, maxAttemptsDefault, tenantID)
		if err != nil {
			if isNotLeader(err) {
				// In raft mode this shard is read-only on this node;
				// try the next one. The wrapper's job is to find
				// claimable work across whichever shards we lead.
				continue
			}
			return nil, false, err
		}
		if ok && t != nil {
			return t, true, nil
		}
	}
	return nil, false, nil
}

func (s *ShardedTaskRepository) ClaimMany(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, max int, inspectLimit int, maxAttemptsDefault int, tenantID string) ([]*domain.Task, error) {
	if max <= 0 {
		return nil, nil
	}
	out := make([]*domain.Task, 0, max)
	start := s.nextStart()
	n := len(s.shards)
	for i := range n {
		if len(out) >= max {
			break
		}
		idx := (start + i) % n
		remain := max - len(out)
		got, err := s.shards[idx].ClaimMany(ctx, workerID, commands, leaseSeconds, remain, inspectLimit, maxAttemptsDefault, tenantID)
		if err != nil {
			if isNotLeader(err) {
				continue
			}
			return out, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (s *ShardedTaskRepository) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	return s.shards[s.shardOf(taskID)].Heartbeat(ctx, taskID, workerID, extendSeconds)
}

func (s *ShardedTaskRepository) Abandon(ctx context.Context, taskID string, workerID string) error {
	return s.shards[s.shardOf(taskID)].Abandon(ctx, taskID, workerID)
}

func (s *ShardedTaskRepository) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	return s.shards[s.shardOf(taskID)].Nack(ctx, taskID, workerID, delaySeconds, maxAttemptsDefault, reason)
}

func (s *ShardedTaskRepository) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	total := 0
	for _, sh := range s.shards {
		n, err := sh.MoveDueDelayed(ctx, cmd, limit)
		if err != nil {
			if isNotLeader(err) {
				continue
			}
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *ShardedTaskRepository) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	var total int64
	for _, sh := range s.shards {
		n, err := sh.PendingLength(ctx, cmd)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *ShardedTaskRepository) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	return s.shards[s.shardOf(taskID)].Get(ctx, taskID)
}

func (s *ShardedTaskRepository) AdminQueues(ctx context.Context) (map[string]any, error) {
	out := make(map[string]any)
	for _, sh := range s.shards {
		m, err := sh.AdminQueues(ctx)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			n, _ := v.(int64)
			prev, _ := out[k].(int64)
			out[k] = prev + n
		}
	}
	return out, nil
}

func (s *ShardedTaskRepository) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
	out := &domain.QueueStats{Command: cmd}
	for _, sh := range s.shards {
		st, err := sh.QueueStats(ctx, cmd, tenantID)
		if err != nil {
			return nil, err
		}
		out.Ready += st.Ready
		out.InProgress += st.InProgress
		out.Delayed += st.Delayed
		out.DLQ += st.DLQ
	}
	return out, nil
}

func (s *ShardedTaskRepository) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	total := 0
	per := limit
	if per <= 0 || per > 100 {
		per = 100
	}
	for _, sh := range s.shards {
		n, err := sh.CleanupExpired(ctx, per, before)
		if err != nil {
			if isNotLeader(err) {
				continue
			}
			return total, err
		}
		total += n
	}
	return total, nil
}

// ---- compile-time check ----
var _ repository.TaskRepository = (*ShardedTaskRepository)(nil)
