package shard

import (
	"context"
	"fmt"

	"github.com/go-redis/redis/v8"
)

// VerifyResult holds the outcome of a post-migration verification.
type VerifyResult struct {
	// SourceCounts maps queue type → remaining count on the source shard.
	SourceCounts map[string]int64
	// DestCounts maps queue type → count on the destination shard.
	DestCounts map[string]int64
	// Healthy maps shard ID → whether a PING succeeded.
	Healthy map[string]bool
	// OK is true when the source has zero remaining tasks and all shards are healthy.
	OK bool
}

// Verify checks that a migration completed correctly.
// It counts remaining tasks in the source shard (should be zero after migration)
// and tasks in the destination shard, and pings all shard connections.
func Verify(ctx context.Context, clients *ClientMap, command, tenantID, fromShard, toShard string) (*VerifyResult, error) {
	if !clients.HasShard(fromShard) {
		return nil, fmt.Errorf("source shard %q not found in client map", fromShard)
	}
	if !clients.HasShard(toShard) {
		return nil, fmt.Errorf("destination shard %q not found in client map", toShard)
	}

	src := clients.Client(fromShard)
	dst := clients.Client(toShard)

	res := &VerifyResult{
		SourceCounts: make(map[string]int64),
		DestCounts:   make(map[string]int64),
		Healthy:      make(map[string]bool),
	}

	// Count tasks on source shard
	srcTotal, err := countQueueTasks(ctx, src, command, tenantID, fromShard)
	if err != nil {
		return nil, fmt.Errorf("count source tasks: %w", err)
	}
	res.SourceCounts = srcTotal

	// Count tasks on destination shard
	dstTotal, err := countQueueTasks(ctx, dst, command, tenantID, toShard)
	if err != nil {
		return nil, fmt.Errorf("count destination tasks: %w", err)
	}
	res.DestCounts = dstTotal

	// Health check all shards
	for _, sid := range clients.ShardIDs() {
		c := clients.Client(sid)
		if err := c.Ping(ctx).Err(); err != nil {
			res.Healthy[sid] = false
		} else {
			res.Healthy[sid] = true
		}
	}

	// Determine overall OK status
	var srcRemaining int64
	for _, v := range res.SourceCounts {
		srcRemaining += v
	}
	allHealthy := true
	for _, h := range res.Healthy {
		if !h {
			allHealthy = false
			break
		}
	}
	res.OK = srcRemaining == 0 && allHealthy

	return res, nil
}

// HealthCheck pings all shard connections and returns a map of shard ID → healthy.
func HealthCheck(ctx context.Context, clients *ClientMap) map[string]bool {
	result := make(map[string]bool)
	for _, sid := range clients.ShardIDs() {
		c := clients.Client(sid)
		if err := c.Ping(ctx).Err(); err != nil {
			result[sid] = false
		} else {
			result[sid] = true
		}
	}
	return result
}

// countQueueTasks counts tasks across all queue types for a given command on a shard.
func countQueueTasks(ctx context.Context, client *redis.Client, command, tenantID, shardID string) (map[string]int64, error) {
	counts := make(map[string]int64)
	pipe := client.Pipeline()

	// Pending queues (priorities 0-9)
	pendingCmds := make([]*redis.IntCmd, 10)
	for pri := 0; pri <= 9; pri++ {
		key := QueueKeyPending(command, tenantID, shardID, pri)
		pendingCmds[pri] = pipe.LLen(ctx, key)
	}

	// Delayed queue
	delayedKey := QueueKeyDelayed(command, tenantID, shardID)
	delayedCmd := pipe.ZCard(ctx, delayedKey)

	// In-progress queue
	inprogKey := QueueKeyInProgress(command, tenantID, shardID)
	inprogCmd := pipe.SCard(ctx, inprogKey)

	// DLQ
	dlqKey := QueueKeyDLQ(command, tenantID, shardID)
	dlqCmd := pipe.SCard(ctx, dlqKey)

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("pipeline count for command=%s shard=%s: %w", command, shardID, err)
	}

	var pendingTotal int64
	for pri := 0; pri <= 9; pri++ {
		n, _ := pendingCmds[pri].Result()
		pendingTotal += n
	}
	counts["pending"] = pendingTotal
	counts["delayed"], _ = delayedCmd.Result()
	counts["inprog"], _ = inprogCmd.Result()
	counts["dlq"], _ = dlqCmd.Result()

	return counts, nil
}
