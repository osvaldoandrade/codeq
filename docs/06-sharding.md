# Sharding

## Current Status

Sharding is not yet implemented in the current codeQ service. All queues operate on a single KVRocks instance, and queue keys do not include shard segments.

## Future Implementation

A comprehensive High-Level Design (HLD) and RFC for queue sharding has been developed to enable horizontal scaling beyond single-node KVRocks deployments. The design addresses:

- **Scaling constraints** when workloads exceed single-instance capacity
- **Sharding strategies** (hash-based, range-based, and explicit sharding)
- **Architecture proposal** with a pluggable ShardSupplier interface
- **Implementation phases** for gradual adoption with rollback points
- **Strategic alternatives** for different operational scenarios

For complete details on the sharding design, including:
- Problem analysis and scaling requirements
- Proposed ShardSupplier interface and static configuration
- Storage backend mapping and key format evolution
- Atomicity implications and Lua script constraints
- Migration strategies and deployment patterns
- Performance characteristics and resource implications

See the full design document: **[docs/24-queue-sharding-hld.md](24-queue-sharding-hld.md)**

## Summary of Recommended Approach

The HLD recommends **explicit sharding** through a configuration-driven ShardSupplier interface, with three strategic implementation paths:

1. **Option 1: Vertical Scaling Only** - Continue scaling single instances (recommended until instance limits)
2. **Option 2: Independent Stacks per Tenant** - Deploy separate codeQ+KVRocks pairs (near-term pragmatic solution)
3. **Option 3: RAFT-Based Consensus** - Implement distributed coordination layer (long-term aspiration)

Organizations should continue with vertical scaling until approaching instance limits, then evaluate Options 2 or 3 based on their specific workload characteristics and operational constraints.
