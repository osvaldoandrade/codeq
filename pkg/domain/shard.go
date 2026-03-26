package domain

import "context"

// ShardSupplier provides shard routing for queue operations.
// Implementations must be thread-safe and deterministic.
type ShardSupplier interface {
	// QueueShards returns all shard identifiers where queues for this command may exist.
	// Used for operations that must inspect multiple shards, such as queue stats aggregation.
	// Returns a single-element slice with the default shard if the command is not recognized.
	QueueShards(ctx context.Context, command string, tenantID string) ([]string, error)

	// CurrentShard returns the shard identifier to use for enqueue and claim operations.
	// Must return a stable, deterministic value for a given command-tenant pair.
	// All API server instances must return identical values for the same input.
	CurrentShard(ctx context.Context, command string, tenantID string) (string, error)
}
