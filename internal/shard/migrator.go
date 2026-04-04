package shard

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// MigrateProgress reports the current state of a shard migration.
type MigrateProgress struct {
	QueueType string // "pending:<priority>", "delayed", "inprog", "dlq"
	Migrated  int64
	Total     int64
}

// MigrateOptions configures a shard migration run.
type MigrateOptions struct {
	Command   string
	TenantID  string
	FromShard string
	ToShard   string
	BatchSize int64
	DryRun    bool
	// OnProgress is called after each batch. May be nil.
	OnProgress func(MigrateProgress)
}

// MigrateResult summarises a completed migration.
type MigrateResult struct {
	PendingMigrated  int64
	DelayedMigrated  int64
	InProgMigrated   int64
	DLQMigrated      int64
	TotalMigrated    int64
	DryRun           bool
	Elapsed          time.Duration
}

// Migrate moves tasks belonging to a command from one shard to another.
// It processes all queue types: pending (priorities 0-9), delayed, in-progress,
// and DLQ. Task data in the shared "codeq:tasks" hash is copied to the
// destination shard, and the task IDs are moved between the appropriate queue
// keys.
//
// In dry-run mode the function counts the tasks that would be moved without
// making any writes.
func Migrate(ctx context.Context, clients *ClientMap, opts MigrateOptions) (*MigrateResult, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}
	if opts.FromShard == opts.ToShard {
		return nil, fmt.Errorf("source and destination shards are the same: %q", opts.FromShard)
	}

	src := clients.Client(opts.FromShard)
	dst := clients.Client(opts.ToShard)
	if src == nil {
		return nil, fmt.Errorf("source shard %q not found", opts.FromShard)
	}
	if dst == nil {
		return nil, fmt.Errorf("destination shard %q not found", opts.ToShard)
	}

	start := time.Now()
	res := &MigrateResult{DryRun: opts.DryRun}

	// Migrate pending queues (priorities 0-9)
	for pri := 0; pri <= 9; pri++ {
		srcKey := QueueKeyPending(opts.Command, opts.TenantID, opts.FromShard, pri)
		dstKey := QueueKeyPending(opts.Command, opts.TenantID, opts.ToShard, pri)
		n, err := migrateList(ctx, src, dst, srcKey, dstKey, opts, fmt.Sprintf("pending:%d", pri))
		if err != nil {
			return res, fmt.Errorf("migrate pending priority %d: %w", pri, err)
		}
		res.PendingMigrated += n
	}

	// Migrate delayed queue (sorted set)
	{
		srcKey := QueueKeyDelayed(opts.Command, opts.TenantID, opts.FromShard)
		dstKey := QueueKeyDelayed(opts.Command, opts.TenantID, opts.ToShard)
		n, err := migrateZSet(ctx, src, dst, srcKey, dstKey, opts, "delayed")
		if err != nil {
			return res, fmt.Errorf("migrate delayed: %w", err)
		}
		res.DelayedMigrated = n
	}

	// Migrate in-progress queue (sorted set with lease scores)
	{
		srcKey := QueueKeyInProgress(opts.Command, opts.TenantID, opts.FromShard)
		dstKey := QueueKeyInProgress(opts.Command, opts.TenantID, opts.ToShard)
		n, err := migrateSet(ctx, src, dst, srcKey, dstKey, opts, "inprog")
		if err != nil {
			return res, fmt.Errorf("migrate in-progress: %w", err)
		}
		res.InProgMigrated = n
	}

	// Migrate DLQ (set)
	{
		srcKey := QueueKeyDLQ(opts.Command, opts.TenantID, opts.FromShard)
		dstKey := QueueKeyDLQ(opts.Command, opts.TenantID, opts.ToShard)
		n, err := migrateSet(ctx, src, dst, srcKey, dstKey, opts, "dlq")
		if err != nil {
			return res, fmt.Errorf("migrate DLQ: %w", err)
		}
		res.DLQMigrated = n
	}

	res.TotalMigrated = res.PendingMigrated + res.DelayedMigrated + res.InProgMigrated + res.DLQMigrated
	res.Elapsed = time.Since(start)
	return res, nil
}

const tasksHashKey = "codeq:tasks"
const ttlIndexKey = "codeq:tasks:ttl"

// migrateList migrates task IDs stored in a Redis LIST (used for pending queues).
// It pops from the source and pushes to the destination in batches, also copying
// the task hash data.
func migrateList(ctx context.Context, src, dst *redis.Client, srcKey, dstKey string, opts MigrateOptions, queueType string) (int64, error) {
	total, err := src.LLen(ctx, srcKey).Result()
	if err != nil {
		return 0, fmt.Errorf("LLEN %s: %w", srcKey, err)
	}
	if total == 0 {
		return 0, nil
	}

	if opts.DryRun {
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: total, Total: total})
		}
		return total, nil
	}

	var migrated int64
	for migrated < total {
		batchEnd := migrated + opts.BatchSize
		if batchEnd > total {
			batchEnd = total
		}
		batchCount := batchEnd - migrated

		// Read batch of task IDs from source list (LRANGE from the right end)
		ids, err := src.LRange(ctx, srcKey, -batchCount, -1).Result()
		if err != nil {
			return migrated, fmt.Errorf("LRANGE %s: %w", srcKey, err)
		}
		if len(ids) == 0 {
			break
		}

		// Copy task data to destination
		if err := copyTaskData(ctx, src, dst, ids); err != nil {
			return migrated, fmt.Errorf("copy task data for %s: %w", srcKey, err)
		}

		// Push IDs to destination (preserving order)
		ifaces := make([]interface{}, len(ids))
		for i, id := range ids {
			ifaces[i] = id
		}
		if err := dst.LPush(ctx, dstKey, ifaces...).Err(); err != nil {
			return migrated, fmt.Errorf("LPUSH %s: %w", dstKey, err)
		}

		// Remove from source
		for range ids {
			if err := src.RPop(ctx, srcKey).Err(); err != nil && err != redis.Nil {
				return migrated, fmt.Errorf("RPOP %s: %w", srcKey, err)
			}
		}

		migrated += int64(len(ids))
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: migrated, Total: total})
		}
	}
	return migrated, nil
}

// migrateZSet migrates task IDs stored in a Redis sorted set (used for delayed queues).
func migrateZSet(ctx context.Context, src, dst *redis.Client, srcKey, dstKey string, opts MigrateOptions, queueType string) (int64, error) {
	total, err := src.ZCard(ctx, srcKey).Result()
	if err != nil {
		return 0, fmt.Errorf("ZCARD %s: %w", srcKey, err)
	}
	if total == 0 {
		return 0, nil
	}

	if opts.DryRun {
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: total, Total: total})
		}
		return total, nil
	}

	var migrated int64
	for migrated < total {
		// Read a batch
		members, err := src.ZRangeWithScores(ctx, srcKey, 0, opts.BatchSize-1).Result()
		if err != nil {
			return migrated, fmt.Errorf("ZRANGEWITHSCORES %s: %w", srcKey, err)
		}
		if len(members) == 0 {
			break
		}

		ids := make([]string, len(members))
		zMembers := make([]*redis.Z, len(members))
		removeMembers := make([]interface{}, len(members))
		for i, m := range members {
			id := fmt.Sprint(m.Member)
			ids[i] = id
			zMembers[i] = &redis.Z{Score: m.Score, Member: id}
			removeMembers[i] = id
		}

		// Copy task data
		if err := copyTaskData(ctx, src, dst, ids); err != nil {
			return migrated, fmt.Errorf("copy task data for %s: %w", srcKey, err)
		}

		// Add to destination sorted set
		if err := dst.ZAdd(ctx, dstKey, zMembers...).Err(); err != nil {
			return migrated, fmt.Errorf("ZADD %s: %w", dstKey, err)
		}

		// Remove from source
		if err := src.ZRem(ctx, srcKey, removeMembers...).Err(); err != nil {
			return migrated, fmt.Errorf("ZREM %s: %w", srcKey, err)
		}

		migrated += int64(len(members))
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: migrated, Total: total})
		}
	}
	return migrated, nil
}

// migrateSet migrates task IDs stored in a Redis SET (used for in-progress and DLQ).
func migrateSet(ctx context.Context, src, dst *redis.Client, srcKey, dstKey string, opts MigrateOptions, queueType string) (int64, error) {
	total, err := src.SCard(ctx, srcKey).Result()
	if err != nil {
		return 0, fmt.Errorf("SCARD %s: %w", srcKey, err)
	}
	if total == 0 {
		return 0, nil
	}

	if opts.DryRun {
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: total, Total: total})
		}
		return total, nil
	}

	var migrated int64
	for migrated < total {
		// Pop a batch from source set
		ids, err := src.SPopN(ctx, srcKey, opts.BatchSize).Result()
		if err != nil {
			return migrated, fmt.Errorf("SPOPN %s: %w", srcKey, err)
		}
		if len(ids) == 0 {
			break
		}

		// Copy task data
		if err := copyTaskData(ctx, src, dst, ids); err != nil {
			return migrated, fmt.Errorf("copy task data for %s: %w", srcKey, err)
		}

		// Add to destination set
		ifaces := make([]interface{}, len(ids))
		for i, id := range ids {
			ifaces[i] = id
		}
		if err := dst.SAdd(ctx, dstKey, ifaces...).Err(); err != nil {
			return migrated, fmt.Errorf("SADD %s: %w", dstKey, err)
		}

		migrated += int64(len(ids))
		if opts.OnProgress != nil {
			opts.OnProgress(MigrateProgress{QueueType: queueType, Migrated: migrated, Total: total})
		}
	}
	return migrated, nil
}

// copyTaskData copies task hash entries and TTL index entries from src to dst
// for the given task IDs. Only copies entries that exist on the source.
func copyTaskData(ctx context.Context, src, dst *redis.Client, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	// Read task data from source
	ifaces := make([]interface{}, len(ids))
	for i, id := range ids {
		ifaces[i] = id
	}
	vals, err := src.HMGet(ctx, tasksHashKey, ids...).Result()
	if err != nil {
		return fmt.Errorf("HMGET %s: %w", tasksHashKey, err)
	}

	// Read TTL scores for these tasks
	pipe := src.Pipeline()
	ttlScoreCmds := make([]*redis.FloatSliceCmd, len(ids))
	for i, id := range ids {
		ttlScoreCmds[i] = pipe.ZMScore(ctx, ttlIndexKey, id)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return fmt.Errorf("pipeline ZMSCORE: %w", err)
	}

	// Write task data to destination
	dstPipe := dst.Pipeline()
	for i, id := range ids {
		if vals[i] == nil {
			continue
		}
		js, ok := vals[i].(string)
		if !ok || js == "" {
			continue
		}
		dstPipe.HSet(ctx, tasksHashKey, id, js)

		// Copy TTL index entry if present
		if ttlScoreCmds[i] != nil {
			scores, _ := ttlScoreCmds[i].Result()
			if len(scores) > 0 && scores[0] > 0 {
				dstPipe.ZAdd(ctx, ttlIndexKey, &redis.Z{Score: scores[0], Member: id})
			}
		}
	}

	if _, err := dstPipe.Exec(ctx); err != nil {
		return fmt.Errorf("pipeline write task data: %w", err)
	}
	return nil
}
