# Sharding

Sharding is not implemented in the current codeQ service. All queues are single-shard and keys do not include a shard segment.

If sharding is introduced later, queue keys will include a shard identifier and a ShardSupplier will map commands to shard lists. This document is a placeholder for that future design.
