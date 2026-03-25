package shard

import (
	"fmt"
	"strings"
)

// ShardKeySegment returns the shard key segment to insert into queue keys.
// Returns empty string for the default shard or empty shard ID (backward compatibility).
func ShardKeySegment(shardID string) string {
	if shardID == "" || shardID == DefaultShardID {
		return ""
	}
	return fmt.Sprintf(":s:%s", shardID)
}

// QueueKeyPending builds the pending queue key with optional shard segment.
func QueueKeyPending(command string, tenantID string, shardID string, priority int) string {
	return buildQueueKey(command, tenantID, shardID, fmt.Sprintf("pending:%d", priority))
}

// QueueKeyInProgress builds the in-progress queue key with optional shard segment.
func QueueKeyInProgress(command string, tenantID string, shardID string) string {
	return buildQueueKey(command, tenantID, shardID, "inprog")
}

// QueueKeyDelayed builds the delayed queue key with optional shard segment.
func QueueKeyDelayed(command string, tenantID string, shardID string) string {
	return buildQueueKey(command, tenantID, shardID, "delayed")
}

// QueueKeyDLQ builds the dead-letter queue key with optional shard segment.
func QueueKeyDLQ(command string, tenantID string, shardID string) string {
	return buildQueueKey(command, tenantID, shardID, "dlq")
}

// buildQueueKey assembles a queue key following the format:
//
//	codeq:q:<command>[:<tenantID>][:s:<shardID>]:<queueType>
//
// The shard segment is omitted for the default shard or empty shard ID,
// preserving backward compatibility with existing key formats.
func buildQueueKey(command string, tenantID string, shardID string, suffix string) string {
	var b strings.Builder
	b.WriteString("codeq:q:")
	b.WriteString(strings.ToLower(command))

	if tenantID != "" {
		b.WriteByte(':')
		b.WriteString(tenantID)
	}

	seg := ShardKeySegment(shardID)
	if seg != "" {
		b.WriteString(seg)
	}

	b.WriteByte(':')
	b.WriteString(suffix)

	return b.String()
}
