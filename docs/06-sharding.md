# Sharding

## Current Status

Sharding is not yet implemented in the codeQ service. All queues currently operate on a single KVRocks instance or Redis-compatible backend. Queue keys use tenant identifiers for logical isolation (`codeq:q:{command}:{tenantID}:{queue-type}:{priority}`) but do not include physical shard identifiers.

## Future Design

A comprehensive High-Level Design (HLD) and RFC for queue sharding has been developed to enable horizontal scaling beyond single-node deployments. The design addresses:

- **Explicit sharding** via a pluggable `ShardSupplier` interface for deterministic routing
- **Phased implementation**: Near-term independent storage backends, evolving toward RAFT-based consensus storage (e.g., TiKV)
- **Preservation of guarantees**: Task scheduling, lease management, and at-least-once delivery semantics
- **Lua script atomicity**: Within single command queues despite distributed storage
- **Migration paths**: From single-shard to multi-shard configurations

### Design Documentation

For complete details on the sharding architecture, evaluation of alternatives, implementation strategy, and migration approach, see:

**[High-Level Design: Queue Sharding for Horizontal Scaling](24-queue-sharding-hld.md)**

### Key Design Decisions

The HLD proposes a **combined approach**:
1. **Near-term**: Explicit sharding with independent KVRocks backends for immediate horizontal scaling
2. **Long-term**: RAFT-backed consensus storage (TiKV, etc.) for strong consistency and automatic failover
3. **Alternative path**: Plugin architecture to decouple persistence layer entirely

This strategy balances operational flexibility with scalability requirements while maintaining backward compatibility with single-instance deployments.

## Scaling Considerations

Until sharding is implemented, vertical scaling of KVRocks is recommended:
- Scale CPU and memory resources on the KVRocks instance
- Monitor storage capacity, CPU saturation, and memory pressure
- Consider read replicas for read-heavy workloads
- See [Performance Tuning Guide](17-performance-tuning.md#sharding-considerations) for detailed guidance

## Related Documentation

- [Queue Sharding HLD/RFC](24-queue-sharding-hld.md) - Complete design specification
- [Architecture Overview](03-architecture.md) - Current system architecture
- [Storage Layout](07-storage-kvrocks.md) - Current KVRocks data structures
- [Performance Tuning](17-performance-tuning.md) - Scaling strategies and optimization
